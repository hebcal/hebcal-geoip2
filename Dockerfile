# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hebcal-geoip2 .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hebcal-geoip2 /usr/local/bin/hebcal-geoip2
# Expects the database to be mounted at /var/lib/GeoIP and, for TCP mode,
# publishes 8090. For Unix-socket mode, mount a shared dir for the socket.
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/hebcal-geoip2"]
CMD ["-db", "/var/lib/GeoIP/GeoLite2-City.mmdb", "-addr", "0.0.0.0:8090"]
