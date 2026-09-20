#!/bin/sh
set -u

# Generate an SPDX SBOM with the digest-pinned Syft, and fail on a bad document
# rather than on a registry that was briefly unreachable.
#
# The step this replaces was one `docker run`, so every reason it could not
# finish looked the same. On 2026-09-20 Docker Hub's auth endpoint reset the
# connection while pulling the pinned Syft image — `read: connection reset by
# peer`, exit 125, eight seconds in — and both observability SBOM jobs went red
# on a commit that changed one documentation file. That is the same shape as the
# npm advisory endpoint incident the day before, and scripts/npm-audit-gate.sh
# carries the same reasoning: a red main that everyone knows to ignore is worse
# than no gate at all.
#
# The discriminator is structural, not a match on error text. A completed run
# leaves a parseable SPDX document naming at least one package, whatever it
# found; a run that never reached a registry leaves no document or an empty one.
# So "no usable document" means the tool did not run, and only that earns a
# retry.
#
# What is NOT softened: a run that produced a document Halro cannot read as SPDX,
# or one naming no packages at all, fails immediately and is not retried. That is
# a corrupt artifact rather than a busy registry, and retrying would only publish
# it later.
#
# --require-registry turns the unavailable case back into a failure after the
# retries are spent. The release workflow passes it: a released artifact must not
# ship without its SBOM, and waiting out a registry incident is the right answer
# there. Branch CI does not.

SYFT_IMAGE="anchore/syft@sha256:bd5357d2cd087f03af748dac24df48bfbc1723080d78f75f69aca1f2d429060e"

usage() {
	echo "usage: $0 <syft-source> <output-file> [--require-registry]" >&2
	echo "  e.g. $0 'registry:prom/prometheus@sha256:...' prometheus.spdx.json" >&2
	exit 2
}

require_registry=0
args=""
for arg in "$@"; do
	case "$arg" in
		--require-registry) require_registry=1 ;;
		-*) echo "unknown flag: $arg" >&2; usage ;;
		*) args="${args:+$args }$arg" ;;
	esac
done

# shellcheck disable=SC2086
set -- $args
[ "$#" -eq 2 ] || usage
source_ref="$1"
output="$2"

# A local image lives in the daemon, so Syft needs the socket to see it. A
# registry reference does not, and a container handed a socket it has no use for
# is a privilege nobody asked for.
socket_mount=""
case "$source_ref" in
	docker:*) socket_mount="-v /var/run/docker.sock:/var/run/docker.sock" ;;
esac

attempts=3
delay=20
attempt=1

while :; do
	rm -f "$output"
	# shellcheck disable=SC2086
	docker run --rm \
		-v "$PWD:/output" \
		$socket_mount \
		"$SYFT_IMAGE" \
		"$source_ref" \
		-o "spdx-json=/output/$(basename "$output")" \
		>/dev/null 2>"$output.stderr"
	run_status=$?

	if [ -s "$output" ]; then
		# A document exists. From here the only question is whether it is one,
		# and that is never a reason to retry.
		verdict=$(jq -r '
			if (.spdxVersion // "") == "" then "malformed\tno spdxVersion"
			elif ((.packages // []) | length) == 0 then "malformed\tno packages"
			else "complete\t" + .spdxVersion + " packages=" + (((.packages // []) | length) | tostring)
			end
		' "$output" 2>/dev/null) || verdict="malformed	output is not JSON"
		state=${verdict%%	*}
		detail=${verdict#*	}
		if [ "$state" = "complete" ]; then
			echo "sbom ($source_ref): $detail"
			rm -f "$output.stderr"
			exit 0
		fi
		echo "::error::sbom ($source_ref) produced a document that is not usable SPDX: ${detail}. This is a bad artifact, not a busy registry, so it is not retried."
		sed -n '1,20p' "$output.stderr" >&2
		rm -f "$output.stderr"
		exit 1
	fi

	reason=$(tr '\n' ' ' <"$output.stderr" 2>/dev/null | cut -c1-300)
	[ -n "$reason" ] || reason="syft exited ${run_status} and wrote no document"
	rm -f "$output" "$output.stderr"

	if [ "$attempt" -ge "$attempts" ]; then
		if [ "$require_registry" -eq 1 ]; then
			echo "::error::sbom ($source_ref) could not be generated after ${attempts} attempts and --require-registry was set; refusing to publish without it. Reason: ${reason}"
			exit 1
		fi
		echo "::warning::sbom ($source_ref) could not be generated after ${attempts} attempts; no SBOM was produced for this run. Reason: ${reason}"
		exit 0
	fi
	echo "sbom ($source_ref): attempt ${attempt}/${attempts} produced no document (${reason}); retrying in ${delay}s" >&2
	sleep "$delay"
	attempt=$((attempt + 1))
	delay=$((delay * 2))
done
