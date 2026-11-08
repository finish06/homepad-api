# --- build ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
# VERSION is written into backups as homepadVersion (for people; nothing reads
# it back). Builds that don't pass it say "dev".
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X gitea.kube.calebdunn.tech/code/homepad-api/internal/api.Version=${VERSION}" -o /out/homepad-api ./cmd/homepad-api

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/homepad-api /homepad-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/homepad-api"]
