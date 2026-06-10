# branchd container image (what the Helm chart runs). pgb stays a host CLI.
# Pure-Go build (modernc.org/sqlite): CGO off, static binary, tiny runtime image.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /branchd ./cmd/branchd

FROM alpine:3.21
COPY --from=build /branchd /usr/local/bin/branchd
ENTRYPOINT ["branchd"]
