.PHONY: build demo local real test eval bench package

build:
	go build -o lagom .

# Simulator + gateway + live demo page. No API keys, no network.
demo: build
	./lagom demo

# Real models running on your machine (LM Studio / Ollama on :1234, see config/local.json).
local: build
	./lagom serve -config config/local.json

# Real cloud models. Needs ANTHROPIC_API_KEY in .env. Spends money: read the README first.
real: build
	./lagom serve -config config/real.json

test:
	go vet ./...
	go test -race -count=1 ./...

# The next two need a gateway running in another terminal (make demo / local / real).
eval:
	./lagom eval -n 100 -c 4 -out eval-report.md

bench:
	./lagom bench -c 8 -n 30000
	./lagom bench -c 8 -n 30000 -stream

# Clean zip for sharing: source, configs, docs. Go project only: no keys, binaries, ledgers, IDE files, video tooling or prep notes.
package:
	rm -f ../lagom-pilot-v1.zip
	cd .. && zip -qr lagom-pilot-v1.zip $(notdir $(CURDIR)) \
	  -x '*/.git/*' '*/.idea/*' '*/.env' '*/data/*' '*/video/*' '*/eval-report.md' '*/lagom' '*/.DS_Store'
	@echo "wrote ../lagom-pilot-v1.zip"; unzip -l ../lagom-pilot-v1.zip | tail -1
