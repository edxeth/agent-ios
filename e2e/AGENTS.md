# e2e

- E2E tests run the CLI binary against fake listings and pages. `listingLDFlags` (e2e/cli_test.go) links it with a private listing and no usbmuxd, so it never recovers the fake iwdp for a USB device of the host.
- `e2e/webkitgtk` (`-tags=webkitgtk`) runs commands against a real WebKitGTK MiniBrowser. When many of its tests fail with "MiniBrowser did not list the page", check the host's free memory and swap before the code: a memory-starved host starts MiniBrowser too slowly.

## Simulator tests (`-tags=simulator`)

- e2e/simulator_test.go tests the shared helpers of internal/tools, the proxy listing helpers and the iwdp transport against real Safari in the iOS Simulator (`make test-e2e`), only on the test page that scripts/sim-setup.sh opened. e2e/simsetup_script_test.go runs that script's page selection, lease and Simulator teardown against fakes on any host; e2e/sim_entry_test.go covers the Makefile targets and the CI setup step.
- Get the page connection from `getSimClient` (e2e/sim_helpers_test.go): it shares one connection through `sync.Once` and checks the device and the page's location on every call. Never open a second connection to the page.
- The env-var check in `getSimClient` is the only legitimate `t.Skip` in the simulator tests.
- Route every destructive step of a simulator test (data deletion, cookie changes, DOM replacement, any other change of the page or its origin) through `simMutate`, which checks the device and the page's location right before it acts. A test that navigates the page restores `AGENT_IOS_SIM_PAGE_URL` before it ends.
- Take the destructive targets of a simulator test (cookie URL and domain, origin) from the checked `AGENT_IOS_SIM_PAGE_URL`, never from page state read earlier. Derive the name of every cookie, database and storage key that a simulator test creates or deletes from `AGENT_IOS_SIM_RUN_TOKEN` (`simTarget.testName`).
