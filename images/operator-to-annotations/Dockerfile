FROM golang:1.27.3-trixie@sha256:2336eda24930976591abea806d9b7d99eeae38238f19755c5deec02facdc1594 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/operator-to-annotations .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/operator-to-annotations /usr/local/bin/operator-to-annotations
ENTRYPOINT ["/usr/local/bin/operator-to-annotations"]
