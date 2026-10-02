FROM golang:1.26-trixie@sha256:b389f1219965d8ba67776b81d17308ab25fa763be3855e5fe63ebcb10e15f3a1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/blobs-to-prometheus .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/blobs-to-prometheus /usr/local/bin/blobs-to-prometheus
ENTRYPOINT ["/usr/local/bin/blobs-to-prometheus"]
