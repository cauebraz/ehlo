FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ehlo ./cmd/ehlo \
 && go build -trimpath -ldflags="-s -w" -o /out/ehlo-sim ./cmd/ehlo-sim \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
LABEL org.opencontainers.image.title="ehlo" \
      org.opencontainers.image.description="Email deliverability monitor and warmup pacer for mail servers" \
      org.opencontainers.image.source="https://github.com/cauebraz/ehlo" \
      org.opencontainers.image.url="https://github.com/cauebraz/ehlo" \
      org.opencontainers.image.documentation="https://github.com/cauebraz/ehlo/tree/main/docs" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/ehlo /ehlo
COPY --from=build /out/ehlo-sim /ehlo-sim
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ehlo"]
CMD ["serve", "-config", "/etc/ehlo/ehlo.yaml"]
