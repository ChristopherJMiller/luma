#!/usr/bin/env bash
# check-hygiene: fail if git holds something this public repo must never hold
# (CLAUDE.md rule 3). .gitignore keeps such files out of `git add`; this
# catches what gets past it (`git add -f`, an extension nobody listed):
#   1. a tracked binary file (git's own text/binary detection);
#   2. a tracked file larger than 512 KiB;
#   3. a tracked file of an archive, key or secret type;
#   4. a secret anywhere in the history (gitleaks; CI checks out the full
#      history for this).
# A path that must break rule 1 or 2 goes in ALLOW below, with a reason.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

max_bytes=$((512 * 1024))
# Paths allowed to be binary or large. Keep it short; every entry needs a
# reason in a comment.
ALLOW=()

allowed() {
	local p
	for p in "${ALLOW[@]}"; do [[ $1 == "$p" ]] && return 0; done
	return 1
}

problems=0
report() {
	echo "check-hygiene: $*" >&2
	problems=$((problems + 1))
}

# 1. Binary files, as git classifies the staged/committed content.
while IFS=$'\t' read -r info path; do
	[[ ${info%% *} == "i/-text" ]] || continue
	allowed "$path" || report "binary file tracked: $path"
done < <(git ls-files --eol)

# 2. Size of each tracked blob.
while IFS=$'\t' read -r meta path; do
	sha=$(cut -d' ' -f2 <<<"$meta")
	size=$(git cat-file -s "$sha")
	if ((size > max_bytes)) && ! allowed "$path"; then
		report "file over $((max_bytes / 1024)) KiB tracked: $path ($size bytes)"
	fi
done < <(git ls-files -s)

# 3. File types that are never source here: archives, raw images, keys and
# certificates, secrets.
forbidden='\.(zip|tar|tgz|tar\.gz|bin|img|pem|key|p12)$|\.secret'
while IFS= read -r path; do
	report "forbidden file type tracked: $path"
done < <(git ls-files | grep -Ei "$forbidden" || true)

# 4. Secrets in any commit.
if ! gitleaks git --no-banner --redact --log-level warn .; then
	report "gitleaks found a secret in the history (rerun: gitleaks git --redact .)"
fi

if ((problems > 0)); then
	echo "check-hygiene: $problems problem(s); see CLAUDE.md rule 3" >&2
	exit 1
fi
echo "check-hygiene: ok ($(git ls-files | wc -l) tracked files; no binaries, big files, keys or secrets)"
