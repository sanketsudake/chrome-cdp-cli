# Worked Examples

## Worked examples

```sh
# See a page, then click a control by a fragment of its verbose name
chrome-cdp use url:workday
chrome-cdp snap --json                                   # find the name / ref
chrome-cdp click --by name "Review" --match contains --role button --json

# Confirm a write via the toast, no screenshot
chrome-cdp click --by name "Approve" --role button --json
chrome-cdp wait --text "Success" --json

# Drive a Workday cascade prompt that click/type can't open
chrome-cdp select "Time Type" "Project Plan > Acme: Widget Platform > Project > Time Entry" --role textbox --json

# Read a grid instead of screenshotting it
chrome-cdp grid --json

# Navigate and wait for the redirect chain to settle
chrome-cdp nav "$APP_URL" --json
chrome-cdp wait --url "/home" --timeout 15s --json

# A click "did nothing" — read what the page said instead of re-clicking
chrome-cdp console --only-errors --json
chrome-cdp net --failed --json

# Confirm a write by its API call when there is no toast to wait on
chrome-cdp click --by name "Save" --role button --json
chrome-cdp wait --request "/api/save" --method POST --status 2xx --json
```

