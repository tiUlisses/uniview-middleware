FROM golang:1.21-alpine AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/univiewd ./cmd/univiewd

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/univiewd /usr/local/bin/univiewd

ENTRYPOINT ["univiewd"]
CMD ["run"]
