FROM golang:1.21 AS builder
WORKDIR /go/src 
ENV GO111MODULE=on  CGO_ENABLED=0   
ADD . .
RUN go mod tidy && go build -tags netgo -o /go/bin/karmada-extend main.go

FROM alpine:3.18.3
RUN apk add --no-cache ca-certificates
RUN apk add --no-cache tzdata
COPY --from=builder /go/bin/karmada-extend /bin/karmada-extend
ENTRYPOINT ["/bin/karmada-extend"]