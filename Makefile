BINARY := hebcal-geoip2
PREFIX ?= /usr/local

.PHONY: all build test vet fmt lint clean install

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	gofmt -l .

clean:
	rm -f $(BINARY)

install: build
	install -Dm0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm0644 systemd/hebcal-geoip2.service \
		$(DESTDIR)/etc/systemd/system/hebcal-geoip2.service
	install -Dm0644 systemd/hebcal-geoip2-reload.path \
		$(DESTDIR)/etc/systemd/system/hebcal-geoip2-reload.path
	install -Dm0644 systemd/hebcal-geoip2-reload.service \
		$(DESTDIR)/etc/systemd/system/hebcal-geoip2-reload.service
