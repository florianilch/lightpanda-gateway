# Gateway for Lightpanda

A gateway service for running Lightpanda workloads in private infrastructure with process lifecycle and concurrency control. If this is not a requirement, see [Lightpanda Cloud](https://lightpanda.io/docs/core-concepts/local-vs-cloud) from the Lightpanda team.

- **Admission control:** Enforce queue-backed concurrency limits with a dedicated child process per browser session.
- **Independent replicas:** Scale capacity by running multiple instances behind a load balancer without cluster coordination, using `/healthz`, `/readyz` and `/metrics`.

---

## Install

Install a supported Lightpanda binary and make sure it is available on `PATH`.

Then install the gateway:

```sh
go install github.com/florianilch/lightpanda-gateway/cmd/lpgw@latest
```

The gateway currently supports Lightpanda `0.4.0`.

## Usage

Set an API key and start the gateway:

```sh
LPGW_API_KEY=secret lpgw
# or use a file
lpgw --api-key-file /path/to/api-key
```

```sh
curl http://localhost:8080/scripts \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer secret" \
  -d '{
    "script": "const p = new Page(); await p.goto(\"$LP_TARGET\"); return p.extract({title: \"h1\"});",
    "secrets": {"LP_TARGET": "https://example.com"},
    "timeout": "30s"
  }'
# {"stdout":"{\"title\":\"Example Domain\"}\n","exit_code":0}

# or send the script directly
curl http://localhost:8080/scripts \
  -H "Content-Type: application/javascript" \
  -H "Authorization: Bearer secret" \
  -d '
    const p = new Page();
    await p.goto("https://example.com");
    return p.extract({title: "h1"});
  '
```

Run `lpgw --help` to see the available settings.

## API

`POST /scripts` runs a PandaScript.

**Authentication:** Bearer token, `X-Api-Key` or `?token=`.

**Other:** `/healthz`, `/readyz`, `/metrics`.

## License

MIT licensed. This project is an independent gateway service and is not affiliated with, endorsed by or sponsored by the Lightpanda team.

Lightpanda is a separate upstream project distributed under its own license.
