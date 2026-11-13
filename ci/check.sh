#!/bin/bash
# The checks a change must pass before it is built for a box, in the order they run. Each
# stops the run if it fails. Run from anywhere: ci/check.sh
#   1. gofmt: nothing to format.
#   2. go vet, and the tests under the race detector.
#   3. A short fuzzing of the PDF writer and the date parser.
#   4. staticcheck: mistakes vet doesn't look for.
#   5. govulncheck: known vulnerabilities in the code that runs, the standard library included.
#   6. The image build (deploy/build.sh), which vets the SANE driver too, runs the tests, and
#      runs the driver code against SANE's test backend.
#   7. Trivy: known vulnerabilities in the image, high and critical ones; and that Trivy found
#      the image's Debian release, without which it checks none of its Debian packages.
#   8. gitleaks: secrets in the files, such as a token or a password left in by mistake.
# The tools are pinned to versions known to exist; raise them on purpose, not by chance.
set -euo pipefail

PROJECT=$(cd "$(dirname "$0")/.." && pwd)
STATICCHECK=honnef.co/go/tools/cmd/staticcheck@2025.1.1
GOVULNCHECK=golang.org/x/vuln/cmd/govulncheck@v1.1.4
TRIVY=aquasec/trivy:0.76.2
GITLEAKS=zricethezav/gitleaks:v8.21.2
SEVERITIES=HIGH,CRITICAL
EXPECTED_DISTRIBUTION=debian
# Prints the distribution of the operating system packages Trivy checked, or nothing.
DISTRIBUTION_TEMPLATE='{{ range . }}{{ if eq .Class "os-pkgs" }}{{ .Type }}{{ end }}{{ end }}'
FUZZ_TIME=30s

cd "$PROJECT"
step() { echo "== $*"; }

step "gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "$unformatted"
	exit 1
fi

step "go vet and the tests, under the race detector"
go vet ./...
go test -race -count=1 ./...

step "fuzzing, $FUZZ_TIME each"
go test -run '^$' -fuzz '^FuzzWriteKeepsItsCrossReference$' -fuzztime "$FUZZ_TIME" ./internal/pdf/
go test -run '^$' -fuzz '^FuzzParseDateReadsWhatItWrites$' -fuzztime "$FUZZ_TIME" ./internal/clock/

step "staticcheck"
go run "$STATICCHECK" ./...

step "govulncheck"
go run "$GOVULNCHECK" ./...

step "the image"
image=$(deploy/build.sh)
echo "$image"

step "Trivy on $image"
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock:ro "$TRIVY" image --exit-code 1 --severity "$SEVERITIES" --ignore-unfixed "$image"
# Trivy knows Debian by /etc/debian_version alone. Without that file, the scan above checks
# only the program and still passes.
distribution=$(docker run --rm -v /var/run/docker.sock:/var/run/docker.sock:ro "$TRIVY" image --quiet --format template --template "$DISTRIBUTION_TEMPLATE" "$image")
if [ "$distribution" != "$EXPECTED_DISTRIBUTION" ]; then
	echo "Trivy found no $EXPECTED_DISTRIBUTION release in $image, so it checked none of its packages"
	exit 1
fi

step "gitleaks"
docker run --rm -v "$PROJECT:/source:ro" "$GITLEAKS" detect --no-git --source /source --exit-code 1

echo "== every check passed: $image"
