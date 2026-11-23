import type { Stage, Status } from './types'

const def: [string, string, string, boolean, string][] = [
  ['spec', 'Spec', 'fliessband/spec', true, 'Contract'], ['scaffold', 'Scaffold', 'templates', true, 'Contract'],
  ['protolint', 'Proto lint', 'buf lint', true, 'Contract'], ['breaking', 'Compatibility', 'buf breaking', true, 'Contract'],
  ['codegen', 'Codegen', 'buf generate', true, 'Contract'], ['implement', 'Implement', 'LLM', false, 'Build'],
  ['policy', 'Policy', 'fliessband/policy', true, 'Build'], ['deps', 'Dependencies', 'go mod tidy', true, 'Build'],
  ['build', 'Build', 'go build', true, 'Build'], ['vet', 'Vet', 'go vet', true, 'Build'],
  ['lint', 'Lint', 'golangci-lint, go fix', false, 'Build'], ['test', 'Test', 'go test -race', true, 'Verify'],
  ['acceptance', 'Acceptance', 'spec examples', true, 'Verify'], ['review', 'Review', 'second-model review', false, 'Verify'],
  ['mutation', 'Mutation', 'fliessband/mutate', false, 'Verify'],
  ['fuzz', 'Fuzz', 'go test -fuzz', false, 'Verify'], ['vulncheck', 'Vulnerabilities', 'govulncheck', false, 'Verify'],
  ['integration', 'Integration', 'PostgreSQL in Docker', true, 'Run'], ['smoke', 'Smoke', 'grpc health + load', true, 'Run'],
  ['sbom', 'Supply chain', 'CycloneDX, SLSA', false, 'Ship'], ['measure', 'Measure', 'fliessband/metrics', false, 'Ship'],
]

/** The line before any run exists. */
export const LINE: Stage[] = def.map(([id, name, tool, gate, phase]) => ({
  id, name, tool, gate, phase, status: 'pending', summary: '', duration_ms: 0, log: [],
}))

export const SHORT: Record<string, string> = {
  spec: 'Spec', scaffold: 'Scaffold', protolint: 'Proto', breaking: 'Compat', codegen: 'Codegen', implement: 'Implement',
  policy: 'Policy', deps: 'Deps', build: 'Build', vet: 'Vet', lint: 'Lint', staticcheck: 'Static', test: 'Test',
  acceptance: 'Accept', review: 'Review', mutation: 'Mutants', fuzz: 'Fuzz', vulncheck: 'Vulns', integration: 'Postgres', smoke: 'Smoke',
  sbom: 'SBOM', measure: 'Measure',
}

export const STATUS_WORD: Record<Status, string> = {
  passed: 'Passed', failed: 'Failed', warning: 'Warning', skipped: 'Skipped', pending: 'Waiting', running: 'Running', canceled: 'Canceled',
}
