// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// ---------------------------------------------------------------------------
// 目标位置: internal/mesh/route_fallback_test.go  (包 mesh, 新文件)
//
// 本文件覆盖设计稿 /tmp/mesh-trunk-design-round50.md §5.2 的前 3 条反向用例:
//   N1  中继不可达 → 回退 direct 并成功
//   N2  中继可达但认证失败 → **不回退**, 明确报错
//   N3  目标不是成员 → **一次都不该走中继**
//
// 【与第 52 轮骨架的区别 —— 这是本文件存在的理由】
//
// 第 52 轮的骨架 (/tmp/trunk-tests-round52/route_fallback_test.go) 定义了
// 三个**在仓库里不存在**的符号:
//
//     type Candidate struct { Kind PathKind; Via string; Err error }
//     type CandidatePicker struct { Candidates []Candidate; RelayCalls int; DirectCalls int }
//     func FallbackAllowed(err error) bool
//
// 也就是说: 那份骨架**在今天的仓库上根本编译不过** —— 它测的是一个
// 平行宇宙里的 picker 实现, 不是 PR-1 落地后的代码。它证明不了任何事。
//
// 本文件全部针对 **PR-1 已经落地的真实符号**:
//     internal/mesh/route.go:53   PathPicker
//     internal/mesh/route.go:92   pathDialer
//     internal/mesh/route.go:108  tryPaths
//     internal/mesh/route.go:139  dialViaPicker
//     internal/mesh/route.go:161  dialPath
//     internal/mesh/errors.go:98  classifyDialError
//     internal/mesh/direct.go:772 preferDialError
//
// 【实测状态 —— 见 /tmp/trunk-pr2-prep-round56.md §4】
//
//   6 条用例, 在当前 HEAD 上:
//     3 条 **红** (relay 还不存在 / 路径层有三处真实缺陷)
//     3 条 **绿** (策略对照组, 锁住已经正确的行为)
//
//   红的 3 条不是"因为 relay 没写所以编译不过", 而是**断言的是行为**,
//   当前代码真的做错了。这才是有效的骨架。
//
// 【铁律: 回退策略必须在生产代码里】
//
// 本文件**不**定义任何回退策略函数, 只调用生产代码里的 tryPaths。
// 策略在 route.go:108 tryPaths 里 (单一决策点)。
// 第 52 轮已经实测过这个陷阱: 把策略写进测试文件 → 变异测试失效
// (改测试等于改被测物, 用例永远全绿)。
//
// 【fixture】全部复用现成的, 不新造:
//   newDirectFixture  internal/mesh/direct_test.go:36
//   startListener      internal/mesh/direct_test.go:60
//   dialTo             internal/mesh/direct_test.go:74
//   deadEndpoint       internal/mesh/direct_fallback_test.go:61
//
//   走真实 QUIC, 不用 net.Pipe(CONVENTIONS.md T-2.8: net.Pipe 不实现
//   CloseWrite, 会走错半关闭分支)。
// ---------------------------------------------------------------------------

package mesh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// --- 工具: 与 route_test.go 里的 fakePicker/fakeDialer 不同名, 避免同包重定义 ---

// countingPicker 记录 Pick 被问了几次, 并返回一条固定的路径表。
//
// 它存在的目的不是"假装中转可用", 而是**观察路径层问了哪些路径**。
// N3 的核心断言就是 "Pick 不该被问" —— 一个非成员目标没有路径可言,
// 问一次就把目标 ID 泄露给了一个本不该参与的节点。
type countingPicker struct {
	paths []Path
	calls int
}

func (p *countingPicker) Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error) {
	p.calls++
	return p.paths, nil
}

// countingDialer 记录每种路径被尝试了几次, 并按脚本返回结果。
//
// 这是本文件里唯一能证明"回退真的发生了"的手段:
// 只证明"direct 能用"是零信息的 —— 一个永不回退的实现也能满足它。
type countingDialer struct {
	fail  map[PathKind]error
	calls map[PathKind]int
}

func (d *countingDialer) dial(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
	if d.calls == nil {
		d.calls = map[PathKind]int{}
	}
	d.calls[p.Kind]++
	if err, ok := d.fail[p.Kind]; ok {
		return nil, err
	}
	return &Link{}, nil
}

// --- N1 -------------------------------------------------------------------

// TestN1_RelayUnreachableFallsBackToDirect (绿 · 对照组)
//
// 中继不可达 → 回退 direct 并成功。
//
// 这是 N1 的**策略对照**: 该退的必须退。第 52 轮已经指出, 只写 N2 的话,
// 一个"永不回退"的实现也能全绿 —— 所以 N1 与 N2 必须成对。
//
// 当前状态: 绿。tryPaths (route.go:108) 在 ErrUnreachable 之后继续走下一条路径。
func TestN1_RelayUnreachableFallsBackToDirect(t *testing.T) {
	paths := []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
		{Kind: PathDirect},
	}
	d := &countingDialer{fail: map[PathKind]error{
		PathRelay: fmt.Errorf("quic dial relay: %w", context.DeadlineExceeded),
	}}
	link, used, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err != nil {
		t.Fatalf("N1: an unreachable relay must fall back to direct, got %v", err)
	}
	if link == nil {
		t.Fatal("N1: tryPaths must return a link")
	}
	if used.Kind != PathDirect {
		t.Fatalf("N1: the fallback must land on PathDirect, got %q", used.Kind)
	}
	if d.calls[PathRelay] != 1 {
		t.Fatalf("N1: the relay path must have been attempted exactly once (relayCalls=%d); "+
			"otherwise this only proves direct works, not that a fallback happened", d.calls[PathRelay])
	}
	if d.calls[PathDirect] != 1 {
		t.Fatalf("N1: direct must have been tried once after the relay failed (directCalls=%d)", d.calls[PathDirect])
	}
}

// TestN1_RelayPathMustNotMasqueradeAsNoRoute (红 · 反向证明)
//
// **当前实现是错的。** route.go:168-169 的 PathRelay 分支返回:
//
//	fmt.Errorf("%w: mesh: relay paths are not available yet", ErrNoRoute)
//
// 问题在于 ErrNoRoute 在本仓的**既定含义**是"成员视图里没有这条路的端点"
// (direct.go:697: "%s has no reachable endpoints in the current view"),
// 也就是**一个路由事实**。而"中转功能还没实现"是**一个实现状态**。
// 两者共用一个 sentinel, 后果是路径层 route.go:123-127 把它当成
// "这条候选不行, 换下一条" —— 于是:
//
//	· 调用方拿到的是 "no route to target", 而真相是 "这条功能没写";
//	· 一个**本该成功**的中转调用, 会被静默降级成"换直连再试一次"。
//
// 这条断言要的就是把这两件事分开: 中转路径的失败**不得**用 ErrNoRoute 表达。
//
// 当前状态: **红**。错误文本含 "not available yet", 且 errors.Is(err, ErrNoRoute) 为真。
//
// 修好之后 (PR-2) 的形态是: 中转路径失败要么成功, 要么带一个**专属**类别
// (网络不可达 / 被拒 / 配额满), 绝不借用 ErrNoRoute。
func TestN1_RelayPathMustNotMasqueradeAsNoRoute(t *testing.T) {
	_, err := dialPath(context.Background(), DialConfig{
		Device:    &identity.Device{},
		Resolver:  MemberListAdapter{},
		Target:    identity.DeviceID("target1111111111"),
		Endpoints: nil,
	}, Path{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")})
	if err == nil {
		t.Fatal("N1: with no relay implementation there must be an error")
	}
	if strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("N1: an unimplemented relay path must not report %q — the caller would "+
			"read it as a routing fact and silently fall back to direct; got %v",
			"not available yet", err)
	}
	if errors.Is(err, ErrNoRoute) {
		t.Fatalf("N1: ErrNoRoute means 'the member view holds no endpoint for this target' "+
			"(direct.go:697); a relay attempt must not borrow it — got %v", err)
	}
}

// --- N2 -------------------------------------------------------------------

// TestN2_AuthFailureDoesNotFallBackToDirect (绿 · 对照组)
//
// 中继可达但认证失败 → **不回退**, 明确报错。
//
// 断言里的第三条是 N2 的全部意义: 把一次安全拒绝降级成静默重试 = 撤销失效。
// 当前状态: 绿。tryPaths (route.go:123) 见到 ErrAuth/ErrRefused 立刻停止。
func TestN2_AuthFailureDoesNotFallBackToDirect(t *testing.T) {
	paths := []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
		{Kind: PathDirect},
	}
	d := &countingDialer{fail: map[PathKind]error{
		PathRelay: fmt.Errorf("relay open: %w", ErrAuth),
	}}
	_, _, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err == nil {
		t.Fatal("N2: an auth failure must surface as an error, not as a successful path")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("N2: the auth class must survive, got %v", err)
	}
	if d.calls[PathDirect] != 0 {
		t.Fatalf("N2: after an auth failure the direct path must NOT be dialled (directCalls=%d); "+
			"falling back here turns a revocation into a silent retry", d.calls[PathDirect])
	}
	if d.calls[PathRelay] != 1 {
		t.Fatalf("N2: the relay path must have been attempted exactly once (relayCalls=%d)", d.calls[PathRelay])
	}
}

// TestN2_RelayRefusalAlsoDoesNotFallBackToDirect (绿 · 对照组)
//
// 协议级拒绝 (对端回答了"不") 同样不回退: 答案换条路不会变。
// 当前状态: 绿。
func TestN2_RelayRefusalAlsoDoesNotFallBackToDirect(t *testing.T) {
	paths := []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
		{Kind: PathDirect},
	}
	d := &countingDialer{fail: map[PathKind]error{
		PathRelay: fmt.Errorf("relay open: %w", ErrRefused),
	}}
	_, _, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err == nil {
		t.Fatal("N2: a protocol-level refusal must surface as an error")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("N2: the refusal class must survive, got %v", err)
	}
	if d.calls[PathDirect] != 0 {
		t.Fatalf("N2: after ErrRefused the direct path must NOT be dialled (directCalls=%d)", d.calls[PathDirect])
	}
}

// TestN2_FailedPathMustBeReportedNotDirect (红 · 反向证明)
//
// **当前实现是错的。** route.go:148-152 的 dialViaPicker:
//
//	link, used, err := tryPaths(ctx, cfg, paths, dialPath)
//	if err != nil {
//	    return nil, PathDirect, err     // ← used 被丢掉了
//	}
//	return link, used.Kind, nil
//
// 失败时**无论死在哪条路径**, 返回的永远是 PathDirect。于是:
//
//	· CLI 报出来的 link= 永远是 "direct", 即使死在一条中转路径上;
//	· 运维看不出"是哪个中转方不行", 无法定位。
//
// 而 CLI 要按设计稿 §5.1 V2 打印 `link=relay via=<deviceID>`, 就必须知道
// 失败发生在哪条路径上 —— 也就是说 **CLI 接线这一项 (设计稿 §4 前置件⑥)
// 依赖本条修复**。这不是可以推后的。
//
// 当前状态: **红**。dialViaPicker 在失败时返回 PathDirect。
//
// 修好之后 (PR-2): 失败时返回**失败的那条路径**的 Kind (以及后续 API 需要时
// 的完整 Path, 好让 CLI 打印 via=)。
func TestN2_FailedPathMustBeReportedNotDirect(t *testing.T) {
	f := newDirectFixture(t)
	picker := &countingPicker{paths: []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}}
	_, kind, err := dialViaPicker(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(), // a real member: the failure is the relay, not the target
		Endpoints: []string{"127.0.0.1:1"},
		Picker:    picker,
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("N2: the relay path cannot succeed yet, so the dial must fail")
	}
	if kind != PathRelay {
		t.Fatalf("N2: the reported path must be the one that FAILED (relay), not %q — "+
			"the CLI prints link= from this value, so a wrong value hides which relay broke", kind)
	}
}

// --- N3 -------------------------------------------------------------------

// TestN3_NonMemberTargetMustNotReachAnyRelayPath (红 · 反向证明)
//
// **当前实现是错的。** route.go:139-152 的 dialViaPicker 先问 picker 要路径,
// 之后才在 DialDirect (direct.go:711-719) 里检查目标是不是成员。
//
// 后果有两个, 都是真的:
//
//	① **类别错了**: 非成员目标应当是 ErrAuth (认证/授权失败, 绝不换路径重试),
//	   但今天走中转路径时, 先撞上 route.go:169 的 ErrNoRoute, 调用方拿到的是
//	   "no route to target" —— 一个**路由事实**, 于是上层可能"换条路再试",
//	   把一次撤销变成了静默重试。这正是 errors.go:17-30 的注释所禁止的。
//	② **多问了一句**: 一个非成员目标**没有路径可言**, 问 picker 一次就把
//	   目标 deviceID 递给了路径层 —— 而路径层下一步就会把它递给一个中转方。
//	   撤销掉的设备, 它的 ID 仍然会发给网内其他成员。
//
// 当前状态: **红**。
//
//	· picker.calls == 1 (今天问了)
//	· errors.Is(err, ErrAuth) == false (今天拿到的是 ErrNoRoute)
//
// 修好之后 (PR-2): dialViaPicker 必须**先**确认目标是成员, 再去问路径;
// 非成员 → ErrAuth, picker 一次都不问。
func TestN3_NonMemberTargetMustNotReachAnyRelayPath(t *testing.T) {
	f := newDirectFixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	picker := &countingPicker{paths: []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}}
	_, _, err = dialViaPicker(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    stranger.ID(),
		Endpoints: []string{"127.0.0.1:1"},
		Picker:    picker,
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("N3: dialling a non-member must fail")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("N3: a non-member target must be classifiable as ErrAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("N3: the actionable reason must survive the sentinel wrapping; got %q", err.Error())
	}
	if picker.calls != 0 {
		t.Fatalf("N3: the path layer must not ask for paths to a non-member (pickerCalls=%d); "+
			"asking hands the target's device ID to the path layer, which hands it to a relay", picker.calls)
	}
}

// TestN3_OutboundOnlyMemberStillReachesTheRelayPath (绿 · 对照组)
//
// 与上一条成对: 上一条说"非成员不该问路径", 这一条说"**纯出站成员必须问**"。
//
// 纯出站成员(零端点)恰恰是**唯一需要中转的一类** (docs/REQUIREMENTS.md R-3.2:
// "纯出站成员（NAT/容器）主动连锚点、经锚点中转")。如果路径层因为
// "没有端点"就拒绝问路径, 中转这条唯一出路就被堵死了。
//
// 判据: 零端点的成员 → ErrNoRoute (不是 ErrAuth) —— 因为凭证没问题,
// 是没有可达地址; 分类错了就会禁止回退 (errors.go:42-43 的注释原话)。
//
// 当前状态: 绿 (direct.go:693-698 已经返回 ErrNoRoute)。
func TestN3_OutboundOnlyMemberStillReachesTheRelayPath(t *testing.T) {
	f := newDirectFixture(t)
	_, err := DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: nil, // pure-outbound member: no endpoints at all
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("N3: a member with no endpoints must not yield a usable link")
	}
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("N3: a member with no endpoints must be ErrNoRoute, not ErrAuth; got %v", err)
	}
	if errors.Is(err, ErrAuth) {
		t.Fatal("N3: a member with no endpoints must NOT be ErrAuth — that would forbid the " +
			"fallback to a relay, which is exactly the path an outbound-only member needs")
	}
}

// TestN3_NonMemberMessageContractSurvives (绿 · 对照组)
//
// 既有契约不能破: 非成员目标的错误文本必须仍然含 "not a member"
// (internal/mesh/direct_test.go:200 断言的就是它)。
// 当前状态: 绿。
func TestN3_NonMemberMessageContractSurvives(t *testing.T) {
	f := newDirectFixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, err = DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    stranger.ID(),
		Endpoints: []string{"127.0.0.1:1"},
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("expected a failure for a non-member target")
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("non-member target must fail with the membership reason; got: %v", err)
	}
}

// TestN3_PreferDialErrorKeepsTheVerdict (绿 · 对照组)
//
// 多端点拨号时, 判定 (对端答了但拒绝) 必须压过网络事实 (没人答)。
// 这是 PR-1 已经落地的 preferDialError (direct.go:772), N2 的语义依赖它:
// 一个被撤销的目标, 第一个端点给了 ErrAuth, 第二个端点是死的 ——
// 若保留最后一个错误, 判定就被抹掉, 路径层会拿同一套被拒的凭证去换路径。
//
// 当前状态: 绿 (internal/mesh/direct_auth_test.go:104 已经覆盖同类,
// 这里补一条 N2 视角的断言, 把"判定优先于网络事实"钉在反向用例名下)。
func TestN3_PreferDialErrorKeepsTheVerdict(t *testing.T) {
	auth := classifyDialError(fmt.Errorf("read ack: %w", ErrAuth))
	unreachable := classifyDialError(fmt.Errorf("quic dial: %w", context.DeadlineExceeded))

	got := preferDialError(auth, unreachable)
	if !errors.Is(got, ErrAuth) {
		t.Fatalf("the auth verdict must win over a later unreachable, got %v", got)
	}
	got = preferDialError(unreachable, auth)
	if !errors.Is(got, ErrAuth) {
		t.Fatalf("the auth verdict must win over an earlier unreachable, got %v", got)
	}
}

// --- 计时辅助 ---------------------------------------------------------------

// 编译期确认: 本文件用到的 DialConfig 字段都存在。
// (DialConfig.Picker 是 PR-1 加的 —— 删掉它, 本文件立刻编译不过,
//
//	这正是"骨架针对新代码"的证明。)
var _ = func(cfg DialConfig) PathPicker { return cfg.Picker }

var _ = time.Second
