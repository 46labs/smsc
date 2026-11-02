# smsc

SMPP 3.4 SMSC mock for local development and tests. It takes transceiver,
transmitter and receiver binds, answers `submit_sm`, sends delivery receipts
and MO as `deliver_sm`, and records every PDU it receives.

Its codec is [gosmpp](https://github.com/linxGnu/gosmpp), so a client built on
another codec is tested against an independent implementation.

## Run

```sh
just run                    # SMPP :2775, control :8080, account pee:secret
docker run -p 2775:2775 -p 8080:8080 -e SMSC_ACCOUNTS=pee:secret ghcr.io/46labs/smsc
```

| Variable | Default |
|---|---|
| `SMSC_ACCOUNTS` | required: `system_id:password` pairs, comma separated |
| `SMSC_ADDR` | `:2775` |
| `SMSC_CONTROL_ADDR` | `:8080` |

## Behaviour

- A bind with an unknown `system_id` gets `ESME_RINVSYSID`, a wrong password
  `ESME_RINVPASWD`.
- `submit_sm` is answered with a message id, or with the next queued error.
- A submit with `registered_delivery` set gets a receipt after the configured
  delay: Appendix B text plus the `receipted_message_id` and `message_state`
  TLVs. Failure-only requests (2) get no `DELIVRD` receipt. A receipt with
  no receiving bind to take it waits for the account's next bind.
- MO longer than one part is split with an 8-bit UDH and `esm_class` 0x40.

## Control API

| Call | Does |
|---|---|
| `GET /healthz` | 204 |
| `GET /mock/pdus?system_id=` | PDUs received |
| `POST /mock/mo` `{system_id, source, destination, text}` | sends MO and returns the status each part was answered with |
| `POST /mock/submit-errors` `{statuses: [88]}` | answers the next submits with these statuses |
| `PUT /mock/receipts` `{state, delay_ms, none}` | receipt state (`DELIVRD`, `UNDELIV`, `EXPIRED`, `REJECTD`, ...) and delay, or none |
| `POST /mock/drop` `{system_id}` | closes the binds |
| `PUT /mock/responses` `{hold: true}` | leaves submits unanswered until released |
| `POST /mock/responses/release` | sends the held responses and stops holding |
| `GET /mock/responses` | `{held: N}`, the responses waiting |
| `POST /mock/reset` | clears records, queued errors, receipt settings, held responses and waiting receipts |

## Test

```sh
just ci
```
