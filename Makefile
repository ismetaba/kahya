# pin; bump deliberately, never 'latest'
SQLC_VERSION ?= v1.30.0
VENV := worker/.venv
PY := $(VENV)/bin/python

.PHONY: build test lint venv generate
build:
	mkdir -p bin
	go build -o bin/kahyad ./kahyad
	go build -o bin/kahya ./kahyad/cmd/kahya
venv:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(PY) -m pip install --quiet -r worker/requirements.lock
test: venv
	go test ./...
	$(PY) -m unittest discover -s worker/tests -v
lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
generate:   # activated by W12-02 when sqlc.yaml lands
	if [ -f sqlc.yaml ]; then go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate; else echo "sqlc.yaml not yet present (W12-02)"; fi
