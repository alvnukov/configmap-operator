FROM golang:1.20.6-alpine3.18 as builder
WORKDIR /app
COPY go.mod .
COPY go.sum .
COPY main.go .
RUN go build -v -o configmap-operator


FROM alpine:3.17.0 as alpine
RUN apk add --no-cache ca-certificates tzdata


FROM  alpine:3.17.0
EXPOSE 9087
WORKDIR /app
COPY --from=alpine /etc/passwd /etc/group /etc/
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=alpine /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /app/configmap-operator /app/configmap-operator
USER nobody
CMD ["/app/configmap-operator"]
