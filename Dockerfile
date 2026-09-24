FROM golang:1.25.4-alpine@sha256:d3f0cf7723f3429e3f9ed846243970b20a2de7bae6a5b66fc5914e228d831bbb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY schema ./schema
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/xrdavies/lite-api/internal/app.Version=${VERSION}" -o /lite-api ./cmd/lite-api

FROM scratch
COPY --from=build /lite-api /lite-api
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
COPY LICENSE COPYING NOTICE /licenses/
ENV ZONEINFO=/zoneinfo.zip LISTEN_ADDR=:8080
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 CMD ["/lite-api", "healthcheck"]
ENTRYPOINT ["/lite-api"]
CMD ["serve"]
