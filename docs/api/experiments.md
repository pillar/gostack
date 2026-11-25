# GameKit experiments API (G12)

Package `kits/game/experiments`, migration `0008_game_experiments.sql`. Client routes under `/v1/games/{game}` (session required), admin routes under `/v1/admin/games/{game}` (admin API key owning the game). Standard error envelope.

## Bucketing and layers
- `Bucket(game, salt, player, n) = uint64(first 8 bytes of SHA-256(game 0x00 salt 0x00 player)) mod n`: a pure function, stable across calls, restarts and servers. Changing it would reshuffle all running experiments (a unit test pins one value).
- Each layer has 10000 slots. Player slot = `Bucket(game, "layer:"+layer, player, 10000)`.
- A running experiment owns the contiguous slice `[slice_start, slice_start + traffic_percent*100)`. When an experiment starts, the slice is placed first-fit among the running experiments of the same layer; if there is no gap that large the request fails with 409 `layer_full`. Slices never overlap, so a player is in at most one running experiment per layer. The slice is fixed while running (growing needs free room at the same start, else `layer_full`; shrinking keeps the start), so starting/stopping other experiments never moves players.
- Variant inside the slice: `Bucket(game, "exp:"+id, player, sum(weights))` walked over the variants in order. Variant choice is independent of the slice position. Salts include the game id, so the same player id buckets differently in each game.
- Eligibility: `platforms` (empty = all) and `min_client_version` (dotted numbers, compared numerically). The client passes `?platform=` and `?client_version=` on the GET and exposure calls; a client that omits `client_version` is not eligible for an experiment with a minimum. Ineligible players are simply not in the experiment.

## Client
- `GET /experiments[?platform=&client_version=]` -> `{"assignments":{"<experiment>":{"layer","variant","params","stopped"?}}}`. Includes running experiments the player falls into, and **stopped** experiments with the control variant and `"stopped":true` (kill switch: everyone is on control). Drafts never appear.
- `POST /experiments/exposures[?platform=&client_version=]` `{"experiment_ids":[1..50 ids]}` -> 204. Records the first exposure per (player, experiment) with the variant at that time; repeats are ignored. Unknown, draft, stopped or not-assigned experiments are silently ignored. Naturally idempotent.

## Admin
- `PUT /experiments/{id}` `{layer, status: draft|running|stopped, variants:[{id, weight 1..10000, params object <=4KiB}] (1-10), control (a variant id), traffic_percent (required, integer 0..100), platforms?, min_client_version?}` -> experiment (running ones include `slice_start`/`slice_end`). Unknown fields are rejected.
  - draft may be edited freely and started (`status:"running"`).
  - running: `layer`, `variants`, `control` are frozen (409 `immutable_while_running`); `traffic_percent`, `platforms`, `min_client_version` may change; back to draft is 409 `invalid_transition`; `status:"stopped"` is the kill switch.
  - stopped is terminal: any `PUT` of a stopped experiment is 409 `experiment_stopped`, even when the body is identical to the stored one (create a new experiment id to rerun).
  - Body limits: `PUT` ≤64 KiB; `POST /experiments/exposures` ≤8 KiB (`413 payload_too_large` above).
- `POST /experiments/{id}/stop` kill switch: status becomes stopped, effective immediately for every request after commit; idempotent (200 on repeat, 404 unknown).
- `GET /experiments`, `GET /experiments/{id}`.
- `DELETE /experiments/{id}` draft or stopped only (409 `experiment_running`); removes its exposures.
- `GET /experiments/{id}/results` -> `{experiment, status, variants:[{variant, control, exposures}], total_exposures}`.

## Module options
`Kit`, `DB`, `Now` (optional). `Maintain` is a no-op (exposures are kept).
