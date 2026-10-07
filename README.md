# Gateway for Lightpanda

A gateway service for running agentic and scripted Lightpanda workloads in private infrastructure with process lifecycle and concurrency control. If this is not a requirement, see [Lightpanda Cloud](https://lightpanda.io/docs/core-concepts/local-vs-cloud) from the Lightpanda team.

- **Admission control:** Enforce concurrency limits with a dedicated child process per browser session.
- **Independent replicas:** Scale capacity by running multiple instances, using `/healthz`, `/readyz` and `/metrics`.

---

## Getting Started

**Base**

Extend the `base` image to supply your own Lightpanda binary:

```dockerfile
FROM ghcr.io/florianilch/lightpanda-gateway:base

# Copy a supported Lightpanda binary into PATH
COPY --from=lightpanda/browser:1.0.0 /bin/lightpanda /usr/local/bin/lightpanda
```

The gateway currently supports Lightpanda `>= 0.4.0`.

**Standalone**

Run the `standalone` image with bundled Lightpanda (or `standalone-distroless`):

```sh
docker run -d --name lpgw -p 8080:8080 \
  -e LPGW_API_KEY=secret \
  ghcr.io/florianilch/lightpanda-gateway:standalone

# or mount the API key from a file
docker run -d --name lpgw -p 8080:8080 \
  -v /path/to/api-key:/run/secrets/api-key:ro \
  -e LPGW_API_KEY_FILE=/run/secrets/api-key \
  ghcr.io/florianilch/lightpanda-gateway:standalone
```

Run `docker run --rm ghcr.io/florianilch/lightpanda-gateway:standalone --help` to see the available settings. All flags have equivalent `LPGW_*` environment variables.

## Usage

### Scripts

```sh
curl http://localhost:8080/scripts \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer secret" \
  -d '{
    "script": "const p = new Page(); await p.goto(\"$LP_TARGET\"); return p.extract({content: \"p\"});",
    "secrets": {"LP_TARGET": "https://example.com"},
    "timeout": "30s"
  }'
# {"stdout":"{\"content\":\"This domain is for use in documentation examples ...\"}","exit_code":0}

# or send the script directly
curl http://localhost:8080/scripts \
  -H "Content-Type: application/javascript" \
  -H "Authorization: Bearer secret" \
  -d '
    const p = new Page();
    await p.goto("https://example.com");
    return p.extract({content: "p"});
  '
```

### Agentic (e.g. Stagehand)

Use Stagehand 3.x. CDP connections in Stagehand 4 require Chrome extension support, which is not implemented.

```ts
import { Stagehand } from '@browserbasehq/stagehand'

const stagehand = new Stagehand({
  env: 'LOCAL', // connect over CDP using the URL below
  localBrowserLaunchOptions: {
    cdpUrl: 'ws://localhost:8080/ws',
    cdpHeaders: { Authorization: 'Bearer secret' },
  },
  model: 'openai/gpt-6-luna',
})

try {
  await stagehand.init()
  const page = await stagehand.context.newPage()
  await page.goto('https://example.com/')

  const result = await stagehand.extract('Extract the main paragraph text from the page')
  console.log(result.extraction)
} finally {
  await stagehand.close()
}
```

### CDP

```ts
import { chromium } from 'playwright-core'

await using browser = await chromium.connectOverCDP(
  'ws://localhost:8080/ws',
  { headers: { 'Authorization': 'Bearer secret' } }
)

const context = await browser.newContext({})
const page = await context.newPage()

await page.goto('https://example.com/')
const content = await page.locator('p:first-of-type').textContent()
console.log(content)

// get page as Markdown
const client = await page.context().newCDPSession(page)
const result = await client.send('LP.getMarkdown', {})
console.log(result.markdown)

await page.close()
await context.close()
```

## API

`POST /scripts` runs a PandaScript.

`GET /ws` starts the browser and exposes its CDP connection over WebSocket.

**Authentication:** Bearer token, `X-Api-Key` or `?token=`.

**Health & Metrics:** `/healthz`, `/readyz`, `/metrics`.

## License

MIT licensed. This project is an independent gateway service and is not affiliated with, endorsed by or sponsored by the Lightpanda team.

Lightpanda is a separate upstream project distributed under its own license.
