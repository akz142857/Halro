import { readFileSync } from 'node:fs';

const advisoryId = 'GHSA-fc5q-39xc-vjpf';
const advisoryUrl = `https://github.com/advisories/${advisoryId}`;

function onlyWithdrawnPicomatch(report, lockfile) {
  const findings = report.vulnerabilities;
  const root = findings?.picomatch;
  if (!root || root.via?.length !== 1 || root.via[0]?.url !== advisoryUrl ||
      root.via[0]?.dependency !== 'picomatch') {
    return false;
  }
  if (!Array.isArray(root.nodes) || root.nodes.length === 0 || !root.nodes.every((node) => {
    const entry = lockfile.packages?.[node];
    return /(^|\/)node_modules\/picomatch$/.test(node) && entry?.dev === true && entry.version === '4.0.7';
  })) {
    return false;
  }

  const names = Object.keys(findings);
  if (names.length !== report.metadata?.vulnerabilities?.total) {
    return false;
  }
  // Every other audit record must be accounted for by the same advisory.
  // Leave the report failing if it contains another advisory or an unknown edge.
  const accountedFor = new Set(['picomatch']);
  while (accountedFor.size < names.length) {
    const previousSize = accountedFor.size;
    for (const name of names) {
      const { via } = findings[name];
      if (Array.isArray(via) && via.length > 0 && via.every((source) => accountedFor.has(source))) {
        accountedFor.add(name);
      }
    }
    if (accountedFor.size === previousSize) {
      return false;
    }
  }
  return true;
}

try {
  const report = JSON.parse(readFileSync(0, 'utf8'));
  const lockfile = JSON.parse(readFileSync(process.argv[2], 'utf8'));
  if (!onlyWithdrawnPicomatch(report, lockfile)) {
    process.exit(1);
  }
  const response = await fetch(`https://api.github.com/advisories/${advisoryId}`, {
    headers: { Accept: 'application/vnd.github+json' },
    signal: AbortSignal.timeout(5000),
    redirect: 'error',
  });
  const advisory = response.ok ? await response.json() : null;
  if (advisory?.ghsa_id !== advisoryId || !(Date.parse(advisory.withdrawn_at) <= Date.now())) {
    process.exit(1);
  }
  console.log(`npm audit: GitHub verified withdrawal of ${advisoryId}; only dev-only picomatch@4.0.7 and its dependents were reported`);
} catch {
  // A missing, malformed, or unavailable confirmation leaves npm's result in force.
  process.exitCode = 1;
}
