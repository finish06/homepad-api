# --- build ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/homepad-api ./cmd/homepad-api

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/homepad-api /homepad-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/homepad-api"]
