// agent-ios agent runtime bundle (design docs/design/agentic-parity.md, section 3.1).
// Files in this directory are joined in name order into one ES2017 script,
// the core. 10-core.js (W3a) defines window.__iwdp with v, call(fn, args),
// def(name, fn), reset, release and has(modules); later files add functions
// with __iwdp.def. Each directory mod-<name>/ is a feature module, built
// into its own script and installed on demand on a core of the same
// version (parity-045 decision D5; internal/runtime Module).
