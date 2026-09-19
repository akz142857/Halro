#!/bin/sh
set -u

# Run `npm audit` as a gate that fails on vulnerabilities but not on an
# advisory endpoint that is down.
#
# `npm audit --audit-level=<level>` exits 1 for two unrelated reasons: it found
# vulnerabilities at or above the level, or it never reached the registry's
# advisory endpoint. Treating both as "the gate failed" turns every npm incident
# into a red main, which is how people learn to ignore a red main. On 2026-09-19
# the registry retired /security/audits/quick while /advisories/bulk answered
# 503, and both of this repo's audit steps failed on a docs-only commit that
# changed no dependency.
#
# The discriminator is structural, not a match on error text: a completed audit
# always carries metadata.vulnerabilities with a count per severity, whatever it
# found. An endpoint error carries an HTTP envelope (message/method/uri) and no
# counts at all. So "no counts" means the audit never ran, and only that case
# earns a retry and a warning. A completed audit that reports vulnerabilities
# fails the job on its first run, exactly as before — the gate is not softened,
# only separated from the registry's availability.
#
# --require-endpoint turns the unavailable case back into a failure after the
# retries are spent. Release workflows pass it: an artifact that reaches users
# must not be published on "the gate did not run", and waiting out an npm
# incident is the right answer there. CI on a branch does not pass it, because
# a red main teaches people to stop reading main.

usage() {
	echo "usage: $0 <directory> [audit-level] [--require-endpoint]" >&2
	exit 2
}

require_endpoint=0
args=""
for arg in "$@"; do
	case "$arg" in
		--require-endpoint) require_endpoint=1 ;;
		-*) echo "unknown flag: $arg" >&2; usage ;;
		*) args="${args:+$args }$arg" ;;
	esac
done

# shellcheck disable=SC2086
set -- $args
[ "$#" -ge 1 ] || usage
directory="$1"
level="${2:-moderate}"

case "$level" in
	info|low|moderate|high|critical) ;;
	*) echo "unknown audit level: $level" >&2; exit 2 ;;
esac

[ -d "$directory" ] || { echo "not a directory: $directory" >&2; exit 2; }

attempts=3
delay=20
attempt=1

while :; do
	report=$(cd "$directory" && npm audit --json 2>/dev/null)

	# node is guaranteed here: every call site has already run `npm ci`.
	verdict=$(printf '%s' "$report" | node -e '
		const level = process.argv[1];
		const order = ["info", "low", "moderate", "high", "critical"];
		let raw = "";
		process.stdin.on("data", (chunk) => { raw += chunk; });
		process.stdin.on("end", () => {
			let parsed;
			try { parsed = JSON.parse(raw); } catch { console.log("unavailable\tunparseable audit output"); return; }
			const counts = parsed && parsed.metadata && parsed.metadata.vulnerabilities;
			if (!counts || typeof counts !== "object") {
				const why = (parsed && (parsed.message || (parsed.error && (parsed.error.summary || parsed.error.detail)))) || "no vulnerability counts in report";
				console.log("unavailable\t" + String(why).replace(/\s+/g, " ").slice(0, 300));
				return;
			}
			const at = order.slice(order.indexOf(level));
			const failing = at.reduce((sum, name) => sum + (Number(counts[name]) || 0), 0);
			const summary = order.map((name) => name + "=" + (Number(counts[name]) || 0)).join(" ");
			console.log((failing > 0 ? "vulnerable" : "clean") + "\t" + summary);
		});
	' "$level")

	state=${verdict%%	*}
	detail=${verdict#*	}

	case "$state" in
		clean)
			echo "npm audit ($directory): no vulnerabilities at $level or above [$detail]"
			exit 0
			;;
		vulnerable)
			echo "npm audit ($directory): vulnerabilities at $level or above [$detail]" >&2
			(cd "$directory" && npm audit --audit-level="$level") || true
			exit 1
			;;
		*)
			if [ "$attempt" -ge "$attempts" ]; then
				if [ "$require_endpoint" -eq 1 ]; then
					echo "::error::npm audit ($directory) could not reach the advisory endpoint after ${attempts} attempts and --require-endpoint was set; refusing to proceed without the gate. Reason: ${detail}"
					echo "npm audit ($directory): endpoint unavailable and required — ${detail}" >&2
					exit 1
				fi
				echo "::warning::npm audit ($directory) could not reach the advisory endpoint after ${attempts} attempts; the vulnerability gate did NOT run. Reason: ${detail}"
				echo "npm audit ($directory): endpoint unavailable, gate skipped — ${detail}" >&2
				exit 0
			fi
			echo "npm audit ($directory): attempt ${attempt}/${attempts} did not reach the advisory endpoint (${detail}); retrying in ${delay}s" >&2
			sleep "$delay"
			attempt=$((attempt + 1))
			delay=$((delay * 2))
			;;
	esac
done
