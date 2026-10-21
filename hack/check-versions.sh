#!/usr/bin/env bash
# check-versions: the toolchain minors and the agent pins must agree.
# Run inside the flake's shell (`nix develop -c make check-versions`); the
# shell's `go` and `node` ARE the flake's versions. Fails unless:
#   1. go.mod's `go` directive has the flake's Go minor;
#   2. the Dockerfile's `golang:` and `node:` base images have the flake's
#      Go and Node minors (skipped while there is no Dockerfile);
#   3. the Agent SDK is 0.3.N and the Claude Code CLI 2.1.N, with the same N
#      (skipped while there is no conductor/package.json);
#   4. pnpm-lock.yaml resolves exactly those pins: every
#      @anthropic-ai/claude-code* package (the CLI and its platform binaries)
#      at the direct CLI pin, and every @anthropic-ai/claude-agent-sdk*
#      package (the SDK and its optional platform packages, each of which
#      bundles a CLI binary) at the SDK pin, so no second CLI version can
#      ride in (S19).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

problems=0
report() {
	echo "check-versions: $*" >&2
	problems=$((problems + 1))
}

# minor <version>: "1.26.8" -> "1.26"
minor() { sed -E 's/^v?([0-9]+\.[0-9]+).*/\1/' <<<"$1"; }

flake_go=$(minor "$(go env GOVERSION | sed 's/^go//')")
flake_node=$(minor "$(node --version)")

# 1. go.mod
mod_go=$(awk '$1 == "go" { print $2; exit }' go.mod)
if [[ -z $mod_go ]]; then
	report "go.mod has no go directive"
elif [[ $(minor "$mod_go") != "$flake_go" ]]; then
	report "go.mod says go $mod_go but the flake's Go is $flake_go"
fi

# 1b. pnpm: the flake's pnpm must be the package.json packageManager version
# (.npmrc turns pnpm's self-download off, so a mismatch would go unnoticed).
pm=$(jq -r '.packageManager // empty' package.json)
if [[ $pm != "pnpm@$(pnpm --version)" ]]; then
	report "package.json packageManager is '$pm' but the flake's pnpm is $(pnpm --version)"
fi

# 2. Dockerfile(s), tracked, anywhere in the tree
mapfile -t dockerfiles < <(git ls-files -- ':(glob)**/Dockerfile*')
for df in "${dockerfiles[@]}"; do
	[[ -f $df ]] || continue
	while read -r image; do
		tag=${image#*:}
		case $image in
		golang:*) want=$flake_go name=Go ;;
		node:*) want=$flake_node name=Node ;;
		esac
		if [[ $(minor "$tag") != "$want" ]]; then
			report "$df uses $image but the flake's $name is $want"
		fi
	done < <(grep -Eio '(^|[[:space:]])(golang|node):[0-9][^[:space:]@]*' "$df" | tr -d ' \t' | sort -u)
done

# 3. SDK and CLI pins
pkg=conductor/package.json
if [[ -f $pkg ]]; then
	sdk=$(jq -r '[.dependencies, .devDependencies] | map(select(. != null) | .["@anthropic-ai/claude-agent-sdk"]) | map(select(. != null)) | first // empty' "$pkg")
	cli=$(jq -r '[.dependencies, .devDependencies] | map(select(. != null) | .["@anthropic-ai/claude-code"]) | map(select(. != null)) | first // empty' "$pkg")
	if [[ ! $sdk =~ ^0\.3\.([0-9]+)$ ]]; then
		report "$pkg: @anthropic-ai/claude-agent-sdk must be pinned exactly as 0.3.N (got '${sdk}')"
	else
		sdk_n=${BASH_REMATCH[1]}
		if [[ ! $cli =~ ^2\.1\.([0-9]+)$ ]]; then
			report "$pkg: @anthropic-ai/claude-code must be pinned exactly as 2.1.N (got '${cli}')"
		elif [[ ${BASH_REMATCH[1]} != "$sdk_n" ]]; then
			report "$pkg: SDK 0.3.$sdk_n and CLI $cli must share the same N"
		fi
	fi

	# 4. The lockfile. `packages` keys are "<name>@<version>"; importer
	# versions may carry a "(peer…)" suffix, which is not part of the version.
	lock=pnpm-lock.yaml
	locked=$(yq '.importers.conductor.dependencies["@anthropic-ai/claude-code"].version // ""' "$lock")
	if [[ $locked != "$cli" ]]; then
		report "$lock: conductor resolves @anthropic-ai/claude-code '$locked', but $pkg pins '$cli'"
	fi
	locked=$(yq '.importers.conductor.dependencies["@anthropic-ai/claude-agent-sdk"].version // ""' "$lock")
	if [[ ${locked%%(*} != "$sdk" ]]; then
		report "$lock: conductor resolves @anthropic-ai/claude-agent-sdk '${locked%%(*}', but $pkg pins '$sdk'"
	fi
	found_cli=0
	while read -r key; do
		name=${key%@*} version=${key##*@}
		case $name in
		@anthropic-ai/claude-code | @anthropic-ai/claude-code-*) want=$cli found_cli=$((found_cli + 1)) ;;
		@anthropic-ai/claude-agent-sdk | @anthropic-ai/claude-agent-sdk-*) want=$sdk ;;
		*) continue ;;
		esac
		if [[ $version != "$want" ]]; then
			report "$lock: $name resolves to $version, not the pinned $want"
		fi
	done < <(yq '.packages | keys | .[]' "$lock")
	if ((found_cli == 0)); then
		report "$lock: no @anthropic-ai/claude-code package is locked"
	fi
fi

if ((problems > 0)); then
	echo "check-versions: $problems problem(s)" >&2
	exit 1
fi
echo "check-versions: ok (go $flake_go, node $flake_node)"
