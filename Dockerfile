FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN test ! -e /src/.local && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/usage-billing ./cmd/usage-billing

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/usage-billing /usage-billing
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usage-billing"]
