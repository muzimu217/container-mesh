# container-mesh relay/server image.
#
# Build:  docker build -t cmesh .
# Run:    docker run -p 8080:8080 cmesh relay --addr :8080
#         docker run -it --rm cmesh send --relay wss://relay.example.com/ws --code ...

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cmesh ./cmd/cm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cmesh /cmesh
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/cmesh"]
# Default subcommand; override with e.g. `send`/`recv`/`probe`.
CMD ["relay", "--addr", ":8080"]
