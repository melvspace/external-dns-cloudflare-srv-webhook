# Stage 1: build
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o webhook .

# Stage 2: minimal runtime
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /app/webhook /webhook

USER 65532:65532

EXPOSE 8888

ENTRYPOINT ["/webhook"]
