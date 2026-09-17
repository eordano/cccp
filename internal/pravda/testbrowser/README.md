# SAE browser fixture

From `projects/cccp`, run these commands in separate terminals:

```sh
GOCACHE=/tmp/pravda-go-cache PRAVDA_BROWSER_MESSAGES=436 PRAVDA_BROWSER_HTMX=/path/to/htmx.min.js go run ./internal/pravda/testbrowser
chromium --headless --no-sandbox --disable-gpu --remote-debugging-port=19222 --user-data-dir=/tmp/pravda-sae-browser-chromium about:blank
node internal/pravda/testbrowser/smoke.mjs
node internal/pravda/testbrowser/loading.mjs
```

Use the htmx 2.0.4 asset pinned in `flake.nix` (SHA-256 `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447`). `PRAVDA_BROWSER_HTMX` lets the fixture serve that asset without modifying source files. Nix normally installs it during the build; it is absent from a plain checkout.

The fixture serves `http://127.0.0.1:18765/session/demo.jsonl`. It creates a temporary database and synthetic transcript: two dictionaries, seven features, overlapping values, values below 35%, imported records without activations, and messages without imported records. The default is 36 messages; the smoke test requires 436 to exercise navigation beyond the timeline's 400-entry window. `PRAVDA_BROWSER_ADDR` overrides the server listen address; set `PRAVDA_BROWSER_URL` for the smoke test to match.

`cdp.mjs` connects to Chromium using Node's built-in WebSocket implementation, with no npm dependencies. Set `CDP_PORT` to override port 19222. Use a dedicated Chromium profile: the smoke test clears browser-local SAE preferences and exercises dictionary switching.

The test reports its assertions, panel height, and JavaScript errors. Screenshots are written to `/tmp/pravda-sae-desktop.png`, `/tmp/pravda-sae-phone.png`, and `/tmp/pravda-sae-settings.png`. Stop the fixture and Chromium after testing.

`loading.mjs` checks navigation and form feedback, overlapping busy states, HTMX completion/error cleanup, SAE refresh success/failure, Escape, and page restoration. It exits nonzero on a failed assertion or JavaScript exception.

`node internal/pravda/testbrowser/ondemand.mjs` uses a synthetic queued → running → stored response in an isolated browser page. It checks automatic polling, exact low activations, event selection, and stopping polling after completion; it needs Chromium but no scoring service.
