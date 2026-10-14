FROM golang:1.26-trixie@sha256:b389f1219965d8ba67776b81d17308ab25fa763be3855e5fe63ebcb10e15f3a1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/operator-to-annotations .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/operator-to-annotations /usr/local/bin/operator-to-annotations
ENTRYPOINT ["/usr/local/bin/operator-to-annotations"]
