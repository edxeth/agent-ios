// Fault injection for TestNodeRunFailureIsReported (after the review
// reproduction of W5c-2a round 1): the script's result file reports a
// script-level failure (ok false and an error) and the script exits 7.
// Loaded with node --import before a client script.
import fs from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';

const write = fs.writeFileSync;
fs.writeFileSync = function (file, data, ...rest) {
  if (String(file).endsWith('/result.json')) {
    const result = JSON.parse(String(data));
    result.ok = false;
    result.error = 'injected script-level failure after connect';
    data = JSON.stringify(result);
  }
  return write.call(this, file, data, ...rest);
};
syncBuiltinESMExports();
const exit = process.exit.bind(process);
process.exit = () => exit(7);
