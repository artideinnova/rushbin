# The team site: rushbin serve. See deploy/compose.yml.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /rushbin . && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /rushbin /rushbin
COPY --from=build --chown=65532:65532 /data /var/lib/rushbin
EXPOSE 8080
ENTRYPOINT ["/rushbin", "serve", "-addr", ":8080", "-data", "/var/lib/rushbin"]
