# Build a static relayd binary and package it minimally.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /relayd ./cmd/relayd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relayd /relayd
EXPOSE 8443
ENTRYPOINT ["/relayd"]
