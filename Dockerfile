FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/iryzhkov/jocasta/internal/buildinfo.Version=$VERSION -X github.com/iryzhkov/jocasta/internal/buildinfo.Commit=$COMMIT" -o /out/jocastad ./cmd/jocastad
FROM alpine:3.22
RUN addgroup -g 10001 jocasta && adduser -D -u 10001 -G jocasta jocasta && mkdir /data /backups && chown jocasta:jocasta /data /backups
COPY --from=build /out/jocastad /jocastad
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/jocastad"]
