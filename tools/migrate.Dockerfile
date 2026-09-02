# Keep the migration CLI outside the application module and rebuild its
# PostgreSQL-only binary with the same patched Go toolchain as the application.
FROM golang:1.26.6-alpine AS build
RUN CGO_ENABLED=0 GOBIN=/out go install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/migrate /migrate
USER 65532:65532
ENTRYPOINT ["/migrate"]
