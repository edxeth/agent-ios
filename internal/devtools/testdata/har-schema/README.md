# Vendored HAR 1.2 JSON schema (test fixture)

These JSON files are the published HAR schema of the npm package
`har-schema` 2.0.0 by Ahmad Nassri (https://github.com/ahmadnassri/har-schema),
ISC license (see LICENSE in this directory). They are copied unchanged from
`lib/*.json` of https://registry.npmjs.org/har-schema/-/har-schema-2.0.0.tgz
(sha256 2e7b84af0567ad7c6a5ddcb5f4c1bcc2bd29d4b089d734f628ac08517081be26,
downloaded 2026-11-07). `index.js` of the package is not copied.

Only the tests of internal/devtools read them (TestHARValidatesAgainstHAR12Schema),
with github.com/google/jsonschema-go. The files declare JSON Schema draft-06,
which that validator does not accept; the test reads them as draft-07, which
gives every keyword these files use (type, properties, required, items,
pattern, format, oneOf, $id, $ref) the same meaning.
