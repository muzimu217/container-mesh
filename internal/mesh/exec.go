// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Built-in run verbs: command-style remote diagnosis WITHOUT process
// execution. Every verb is implemented natively in this file — no os/exec,
// no shell — so the attack surface is exactly the verbs below, all
// read-only. Arbitrary command execution is a separate, operator-gated
// feature (pending), not part of this build.
//
// Verbs (request argv[0], prefixed @):
//
//	@info            host identity: hostname/OS/arch/uptime/version
//	@disk [PATH]     filesystem usage for PATH (default /)
//	@mem             memory summary (Linux: /proc/meminfo)
//	@cat <FILE>      read one file, cap 64 MiB
//	@ls [DIR]        directory listing with sizes
//
// Only approved members can reach these (mTLS pinning); verbs are
// read-only by construction.
package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

// Run session constants.
const (
	// KindRun is the built-in-verb session kind.
	KindRun = "run"
	// maxRunFrame bounds one output chunk.
	maxRunFrame = 256 << 10
	// maxRunArgv bounds the request line.
	maxRunArgv = 16 << 10
	// maxRunCat caps @cat file reads.
	maxRunCat = 64 << 20
	// runKillGrace bounds the abort watcher's read wait.
	runKillGrace = 10 * time.Second
)

// RunRequest is the header line: verb + args.
type RunRequest struct {
	Verb string   `json:"verb"`
	Args []string `json:"args,omitempty"`
}

// Run frame tags (first byte of each frame body).
const (
	RunTagOut  = 'O'
	RunTagErr  = 'E'
	RunTagExit = 'X'
)

func writeRunFrame(w io.Writer, tag byte, payload []byte) error {
	return writeFrame(w, append([]byte{tag}, payload...))
}

func writeRunExit(w io.Writer, code int) error {
	var p [5]byte
	p[0] = RunTagExit
	binary.BigEndian.PutUint32(p[1:], uint32(code))
	return writeFrame(w, p[:])
}

func readRunFrame(br *bufio.Reader) (byte, []byte, error) {
	frame, err := readFrame(br, maxRunFrame)
	if err != nil {
		return 0, nil, err
	}
	if len(frame) == 0 {
		return 0, nil, errors.New("mesh: empty run frame")
	}
	return frame[0], frame[1:], nil
}

// receiveRun dispatches a built-in verb. The peer identity is stamped into
// every log-worthy surface by the caller; verbs themselves are read-only.
func receiveRun(br *bufio.Reader, st *quic.Stream, cfg ListenerConfig) error {
	line, err := readLine(br, maxRunArgv)
	if err != nil {
		return err
	}
	var req RunRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errors.New("mesh: bad run request")
	}
	out := func(s string) { _ = writeRunFrame(st, RunTagOut, []byte(s)) }
	fail := func(code int, msg string) {
		_ = writeRunFrame(st, RunTagErr, []byte(msg))
		_ = writeRunExit(st, code)
	}

	switch req.Verb {
	case "@info":
		host, _ := os.Hostname()
		out(fmt.Sprintf("hostname  %s\nos       %s/%s\nversion  %s\ngo       %s\n",
			host, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.Version()))
		if b, err := os.ReadFile("/proc/uptime"); err == nil {
			if secs, perr := strconv.ParseFloat(strings.Fields(string(b))[0], 64); perr == nil {
				out(fmt.Sprintf("uptime   %.0f min\n", secs/60))
			}
		}
	case "@disk":
		path := "/"
		if len(req.Args) > 0 {
			path = req.Args[0]
		}
		var st fsStat
		if err := statFS(path, &st); err != nil {
			fail(1, "disk: "+err.Error())
			return nil
		}
		out(fmt.Sprintf("path     %s\ntotal    %d MiB\nfree     %d MiB\nused     %d MiB\n",
			path, st.totalMiB, st.freeMiB, st.totalMiB-st.freeMiB))
	case "@mem":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			fail(1, "mem: "+err.Error())
			return nil
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(ln, "MemTotal") || strings.HasPrefix(ln, "MemAvailable") ||
				strings.HasPrefix(ln, "SwapTotal") || strings.HasPrefix(ln, "SwapFree") {
				out(ln + "\n")
			}
		}
	case "@cat":
		if len(req.Args) == 0 {
			fail(2, "cat: needs a file path")
			return nil
		}
		p := filepath.Clean(req.Args[0])
		fi, err := os.Stat(p)
		if err != nil {
			fail(1, "cat: "+err.Error())
			return nil
		}
		if fi.Size() > maxRunCat {
			fail(1, fmt.Sprintf("cat: %s exceeds %d MiB cap", p, maxRunCat/(1<<20)))
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			fail(1, "cat: "+err.Error())
			return nil
		}
		defer f.Close()
		rd := bufio.NewReader(io.LimitReader(f, maxRunCat))
		buf := make([]byte, 32<<10)
		for {
			n, rerr := rd.Read(buf)
			if n > 0 {
				if err := writeRunFrame(st, RunTagOut, buf[:n]); err != nil {
					return err
				}
			}
			if rerr != nil {
				break
			}
		}
	case "@ls":
		dir := "."
		if len(req.Args) > 0 {
			dir = req.Args[0]
		}
		ents, err := os.ReadDir(filepath.Clean(dir))
		if err != nil {
			fail(1, "ls: "+err.Error())
			return nil
		}
		for _, e := range ents {
			size := int64(0)
			if !e.IsDir() {
				if fi, err := e.Info(); err == nil {
					size = fi.Size()
				}
			}
			kind := "f"
			if e.IsDir() {
				kind = "d"
			}
			out(fmt.Sprintf("%s %10d  %s\n", kind, size, e.Name()))
		}
	default:
		fail(2, fmt.Sprintf("unknown verb %q (built-ins: @info @disk @mem @cat @ls)", req.Verb))
	}
	_ = writeRunExit(st, 0)
	return nil
}

// SendRun invokes a built-in verb on a member and streams the output to this
// process's stdout, returning the remote exit code.
func SendRun(ctx context.Context, cfg DialConfig, verb string, args []string) (int, error) {
	if !strings.HasPrefix(verb, "@") {
		return 2, fmt.Errorf("mesh: run takes built-in verbs (@info @disk @mem @cat @ls), got %q", verb)
	}
	cfg.Kind = KindRun
	link, err := DialDirect(ctx, cfg)
	if err != nil {
		return 2, err
	}
	defer link.Close()

	req, err := json.Marshal(RunRequest{Verb: verb, Args: args})
	if err != nil {
		return 2, err
	}
	if _, err := fmt.Fprintf(link.Stream(), "%s\n", req); err != nil {
		return 2, fmt.Errorf("mesh: send request: %w", err)
	}

	for {
		tag, payload, err := readRunFrame(link.br)
		if err != nil {
			return 2, fmt.Errorf("mesh: run stream ended before exit frame: %w", err)
		}
		switch tag {
		case RunTagOut:
			_, _ = os.Stdout.Write(payload)
		case RunTagErr:
			_, _ = os.Stderr.Write(payload)
		case RunTagExit:
			if len(payload) < 4 {
				return 2, errors.New("mesh: bad exit frame")
			}
			return int(binary.BigEndian.Uint32(payload)), nil
		default:
			return 2, fmt.Errorf("mesh: unknown run frame tag %q", tag)
		}
	}
}

var _ = time.Second // reserved for verb timeout knobs
