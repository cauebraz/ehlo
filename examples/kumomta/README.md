# KumoMTA to ehlo

KumoMTA posts its log records to `POST /v1/webhooks/kumomta` through a log hook. Checked against KumoMTA release `2026.09.29-b90d8bc1`: the [log record](https://docs.kumomta.com/reference/log_record/), [webhooks](https://docs.kumomta.com/userguide/operation/webhooks/) and `policy-extras/log_hooks.lua`. The mapping is in [docs/events.md](../../docs/events.md#kumomta).

## One record per request

Add to `init.lua`, before the queues helper:

```lua
local log_hooks = require 'policy-extras.log_hooks'

log_hooks:new_json {
  name = 'ehlo',
  url = 'https://ehlo.example.com/v1/webhooks/kumomta?token=REPLACE_WITH_TOKEN',
}
```

`new_json` sends each record as its own JSON object and sets no custom header, so the token travels in the query string. Read it from your secret store when you build the URL.

## Batched

A hook with `batch_size` posts a JSON array, one request per batch. ehlo takes at most `max_body_bytes` (4 MiB by default), 10,000 records and 10,000 events per request. A record gives up to three events per recipient domain, so keep `batch_size` x 3 x domains per message under 10,000.

```lua
local log_hooks = require 'policy-extras.log_hooks'

log_hooks:new {
  name = 'ehlo',
  batch_size = 100,
  constructor = function(domain, tenant, campaign)
    local connection = {}
    local client = kumo.http.build_client {}
    function connection:send_batch(messages)
      local payload = {}
      for _, msg in ipairs(messages) do
        table.insert(payload, msg:get_meta 'log_record')
      end
      local response = client
        :post('https://ehlo.example.com/v1/webhooks/kumomta?token=REPLACE_WITH_TOKEN')
        :header('Content-Type', 'application/json')
        :body(kumo.serde.json_encode(payload))
        :send()
      local disposition = string.format('%d %s: %s', response:status_code(), response:status_reason(), response:text())
      if response:status_is_success() then
        return disposition
      end
      local code = response:status_code()
      if response:status_is_server_error() or code == 429 or code == 401 or code == 403 then
        kumo.reject(400, disposition)
      end
      kumo.reject(500, disposition)
    end
    function connection:close()
      client:close()
    end
    return connection
  end,
}
```

`send_batch` retries a batch on a 5xx, 429, 401 or 403 answer with `kumo.reject(400, ...)`, and on a network error, which raises a Lua error; KumoMTA retries each record later, until it expires ([webhooks guide](https://docs.kumomta.com/userguide/operation/webhooks/)). A 401 or 403 means a wrong token, which an operator fixes, so the records wait instead of being lost. Any other 4xx means ehlo will refuse the same batch again: 400 for a malformed batch or one with no valid event, 413 for a batch over a limit. `kumo.reject(500, ...)` drops it, and KumoMTA logs a `Bounce` for each record with ehlo's answer as the response. The `new_json` helper drops on every failure.

Give the hook's queue its own shaping entry, as the KumoMTA webhook guide says, so delivery limits for mail do not slow it:

```toml
['ehlo.log_hook']
mx_rollup = false
connection_limit = 10
max_deliveries_per_connection = 100000
```

## Source

`egress_source` becomes the ehlo `source`. With more than `max_sources` (256) egress sources, add `&source=egress_pool` to the URL so each pool is one source, or raise `max_sources`. Names must be 1-64 letters, digits, `.`, `_`, `:` or `-`. A record without the chosen field gives no events: `OOB` records never count, `Expiration` records count only with `&source=egress_pool`, and complaints need the trace setup in [docs/integrations.md](../../docs/integrations.md#attribute-kumomta-complaints).

## Try it

`records.jsonl` holds one record of each kind ehlo reads. With ehlo on port 8080:

```console
$ curl -s --data-binary @examples/kumomta/records.jsonl http://127.0.0.1:8080/v1/webhooks/kumomta
{"accepted":12,"invalid":0}
```
