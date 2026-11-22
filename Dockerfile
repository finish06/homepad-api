# --- build ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
# VERSION is written into backups as homepadVersion (for people; nothing reads
# it back). The GHCR build passes VERSION. The Gitea builds go through
# ci-shared's build-push.sh, which always passes GIT_SHA and passes
# APP_VERSION when the workflow sets it (staging-release sets it to the
# `git describe` of main, e.g. 18.0.0-3-gabc1234). The first one set wins:
# VERSION, then APP_VERSION, then dev-<GIT_SHA>, then "dev" (homepad#613).
ARG VERSION=dev
ARG APP_VERSION=
ARG GIT_SHA=
RUN v="$VERSION"; \
    if [ "$v" = dev ] && [ -n "$APP_VERSION" ]; then v="$APP_VERSION"; fi; \
    if [ "$v" = dev ] && [ -n "$GIT_SHA" ]; then v="dev-$GIT_SHA"; fi; \
    echo "homepadVersion: $v"; \
    go build -trimpath -ldflags="-s -w -X gitea.kube.calebdunn.tech/code/homepad-api/internal/api.Version=$v" -o /out/homepad-api ./cmd/homepad-api

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/homepad-api /homepad-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/homepad-api"]
