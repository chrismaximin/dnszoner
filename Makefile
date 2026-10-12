.PHONY: build install test screenshots clean

# make: build ./dnszoner
build:
	go build -o dnszoner .

# make install: put dnszoner in $(go env GOPATH)/bin (or $GOBIN)
install:
	go install .

test:
	go vet ./...
	go test ./...

# Regenerates the README images; see scripts/screenshots.sh for what it needs.
screenshots:
	scripts/screenshots.sh

clean:
	rm -f dnszoner
