# matrix-redditchat-bridge

A Matrix ↔ Reddit chat puppeting bridge, built on [mautrix-go]'s `bridgev2` framework.

Reddit chat runs on a heavily modified, unfederated Dendrite. It speaks enough of the Matrix
client-server API that a normal Matrix client can log into it with a Reddit chat access token, so
this bridge's "network client" is simply a second `mautrix.Client` pointed at Reddit's homeserver
(see `pkg/redditchat`). Everything Reddit-specific is confined to that package.

[mautrix-go]: https://github.com/mautrix/mautrix-go

## Status

v1 scope, working:

- Multiple Matrix users, each logging in with their own Reddit account.
- Automatic chat token refresh, so logins survive Reddit's 24-hour token lifetime.
- Ghost users (`@redditchat_t2_*`) for Reddit accounts, with display name and avatar.
- The bridge bot creates portal rooms, invites the Matrix user, and joins the ghosts.
- Text messages in both directions.
- Images in both directions, re-hosted on each side.
- Replies, which Reddit models as Matrix threads.
- Reddit → Matrix history backfill.
- Double puppeting, via a second appservice or per-user `login-matrix`.

Not implemented yet: reactions, edits, redactions, typing notifications and read receipts. Unsupported message types are logged and dropped rather than half-bridged, and Matrix
clients are told via room capabilities what is and isn't supported.

**Media is images only, in both directions.** Reddit's media endpoint accepts `image/jpeg`,
`image/png`, `image/gif` and `image/webp` and refuses everything else with
`"<type>" is not supported format` - verified for text, PDF, video and octet-stream. It also
validates the bytes rather than trusting the declared type, so a mislabelled file is rejected.
Limits come from Reddit's own `/_matrix/media/v3/config`: **20 MB**, or **100 MB for GIFs**. Both
are enforced before upload so the user gets a clear error rather than a failure from Reddit.

Reddit's `mxc://reddit.com/...` URIs only resolve on Reddit's server (the download 308-redirects
to `i.redd.it`), so incoming images are fetched and re-uploaded to the local homeserver, and
outgoing ones are uploaded to Reddit. Reddit's `com.reddit.nsfw_image` flag is carried through;
its blurred-variant URI is dropped, since it would be a dead link in a Matrix client.

## Building

```sh
./build.sh
```

Requires Go 1.25+. CGO is on by default because mautrix-go's Matrix-side encryption support needs
it; build with `CGO_ENABLED=0` if you don't want encryption.

## Setup

### 1. Config

```sh
./matrix-redditchat -e -c config.yaml
```

Then edit `config.yaml`:

- `homeserver.address` and `homeserver.domain` — your own Matrix homeserver.
- `network.homeserver_url` and `network.server_name` — Reddit's Matrix server. The defaults
  (`https://matrix.redditspace.com` and `reddit.com`) match what Reddit's web client uses.
- `bridge.kick_matrix_users: false` is recommended. Reddit users can't leave chats, and this
  bridge deliberately never pushes Matrix membership changes to Reddit.

### 2. Registration

```sh
./matrix-redditchat -g -c config.yaml -r registration.yaml
```

This writes `registration.yaml` and saves the generated `as_token`/`hs_token` back into
`config.yaml`. Install `registration.yaml` on your homeserver and restart it. See the
[mautrix docs on registering appservices][reg-docs].

[reg-docs]: https://docs.mau.fi/bridges/general/registering-appservices.html

### 3. Double puppeting (optional but recommended)

Double puppeting makes messages you send from Reddit's own web UI appear in the portal as *your*
Matrix user rather than as your ghost.

**Second appservice (recommended).** Generate a second registration whose only job is to hold a
token the bridge can use to masquerade as users on your homeserver:

```sh
./matrix-redditchat --generate-doublepuppet-registration -c config.yaml
```

This writes `doublepuppet-registration.yaml` and prints the `double_puppet.secrets` block to paste
into `config.yaml`. Install this registration on your homeserver too, then restart it.

**Manual per-user.** Users can instead run `!rc login-matrix <access-token>` with their own
access token. Useful for users on homeservers you don't control.

> **Note:** the two mechanisms are not additive per user. The bridge checks
> `double_puppet.secrets` for the user's homeserver *first*, so once the second appservice is
> installed for your domain, local users get appservice double puppeting and any token they set
> with `login-matrix` is bypassed. `login-matrix` still applies to users on other homeservers.

### 4. Run and log in

```sh
./matrix-redditchat -c config.yaml
```

Start a DM with the bridge bot (`@redditchatbot:yourdomain` by default) and run:

```
!rc login
```

There are two login flows, because **Reddit chat tokens are JWTs that expire exactly 24 hours
after they're issued** (`exp == iat + 86400`).

**`cookie` (recommended).** You paste the `Cookie` header from a logged-in Reddit web session,
and the bridge mints chat tokens itself by loading `https://www.reddit.com/chat/`, which returns
a fresh 24-hour token both as a `token_v2` cookie and in the page's `<rs-app>` element. It
refreshes an hour before expiry and again immediately if Reddit rejects a token early, so you
don't have to log in daily. Only `reddit_session` really matters and it lasts about six months.
To get the cookie: open reddit.com logged in, devtools → Network, load `https://www.reddit.com/chat/`,
click a request to `www.reddit.com`, copy the whole `Cookie` request header value.

**`token`.** You paste a single chat token, taken from the `token` attribute of the `<rs-app>`
element in the `/chat` page HTML, or from the `Authorization: Bearer` header of any request to
`matrix.redditspace.com`. Simpler, no cookie stored, but it dies after 24 hours and you must
redo it manually.

Either way the bridge never calls Reddit's logout endpoint, so `!rc logout` only removes local
state and leaves your Reddit session alone.

Useful commands: `!rc login`, `!rc logout`, `!rc list`, `!rc ping-matrix`, `!rc help`.

### Token expiry handling

- The login confirmation tells you when the token expires, or that refresh is active.
- An already-expired token is rejected at login with its expiry time, rather than producing a
  confusing rejection from Reddit.
- With a cookie: refresh happens an hour before expiry, and a token error triggers one forced
  refresh and retry before the sync loop treats it as a failure.
- Without a cookie: the management room gets a warning 30 minutes before expiry, and at expiry
  the login goes to `BAD_CREDENTIALS` and the sync loop stops cleanly rather than hammering
  Reddit with requests that can only fail.

### If token refresh is blocked

Reddit's edge serves a `blocked by network security` 403 to **anonymous** requests from some
networks, on `www.reddit.com` and `oauth.reddit.com` alike. Authenticated requests are exempt —
verified: an unauthenticated request from a blocked host gets the 403 page, while the same host
sending a valid session cookie gets a normal 200 and a fresh token. So a 403 from a plain `curl`
does not mean refresh will fail.

If refresh really is blocked for you, set `network.refresh_proxy_url` to an `http://`, `https://`
or `socks5://` proxy. Only the refresh request is proxied; Matrix traffic still goes direct to
`matrix.redditspace.com`, which is not subject to this block. Refresh errors distinguish the
block page (`ErrRedditBlocked`, which names the proxy setting) from an expired session cookie, so
the logs tell you which one you have.

## Design notes

**Portals are per-login.** Reddit room IDs are globally unique, so a shared portal would be
possible, but if two bridged Reddit accounts are in the same Reddit chat it becomes ambiguous
which account's token should relay a Matrix message. Every portal key therefore carries the login
ID as its receiver.

**Reddit's `invite` membership is a chat request, not a Matrix invite.** Reddit has no invite
concept in its UI. What Matrix reports as `invite` membership means only "someone started a chat
with you and you have never replied" — Reddit flips you to `join` on your first message. Verified
across a live account: of 20 such rooms, the user had sent a message in exactly zero.

These are bridged as **message requests** (`ChatInfo.MessageRequest`), not as invites:

- The portal is created and the history is readable, because Reddit allows `/messages` on an
  unaccepted chat.
- The bridge does **not** join. A join is visible to whoever started the chat, so auto-joining
  would silently accept every pending request on the account.
- Replying accepts it. bridgev2 calls `HandleMatrixAcceptMessageRequest` implicitly on the first
  outgoing message, which joins the Reddit room — the only place the bridge ever joins.

**Dismissed and spam chats are skipped.** Reddit shows a chat request only when it is neither
dismissed nor classified as spam, and the bridge matches that exactly:

| Signal | Where it lives |
| --- | --- |
| Dismissed ("Ignore") | `com.reddit.hidden_chat` room account data, `{"hidden": true}` |
| Spam | newest `com.reddit.invite_spam_status` timeline event, `{"status": "spam"}` |

Pressing Ignore on Reddit changes **nothing else** — room, membership and timeline are
byte-identical before and after, verified by diffing. That account data key is the only trace, and
without checking it the bridge would resurrect every chat the user ever dismissed. On the test
account this is the difference between **16 portals and 40**. Both checks are configurable
(`network.bridge_hidden_chats`, `network.bridge_spam_chats`), default off.

Chats dismissed *after* being accepted carry the same flag, and for those it arrives in sync
account data, so no extra request is needed.

A chat has no room at all until the first message is sent — opening a compose window on Reddit
(`/chat/user/t2_…`) creates nothing server-side, so the bridge never sees empty draft rooms.

**Reddit's sync room list is capped.** `/sync` returns at most 20 joined and 20 invited rooms and
no filter lifts it, so an account with more chats would silently lose the rest with no error to
explain it. After the first sync the bridge lists `/joined_rooms` (uncapped) and bridges anything
missing. Pending requests have no equivalent endpoint and stay capped at 20.

**Unaccepted chats are invisible to `/sync`.** Reddit never reports timeline activity in a room
you've only been invited to. Verified: a message sent to a pending request was readable through
`/messages` within a second, while six minutes of incremental syncs never mentioned the room.
Without a workaround a bridged chat request would show its first message and then go silent until
you replied, silently dropping everything in between. The bridge therefore re-polls unaccepted
chats on a timer (`network.pending_request_poll_interval`, default 5m, 0 disables). Accepted chats
come through sync normally and are never polled. Duplicates are dropped by the same Reddit
event ID check used everywhere else.

Dismissing is also permanent: a new message does **not** clear `com.reddit.hidden_chat`, so
someone you ignored cannot push their way back into your Matrix account by messaging again.

**Matrix leaves are never bridged.** Leaving a portal room on Matrix does nothing on Reddit,
because Reddit has no working leave.

**Deduplication.** Reddit echoes the bridge's own sent messages back through `/sync`. Messages are
stored under their Reddit event ID, and the echo carries the same ID, so bridgev2's existing
duplicate check drops it.

**Requests mirror Reddit's own client.** The sync filter is sent inline as JSON (not registered
via `/user/{id}/filter`), lazy-loads members, and excludes Reddit's internal
`com.reddit.review_open`/`com.reddit.review_close` events. `pkg/redditchat/client_test.go` pins
these request shapes against a stub server so they don't silently drift.

**Reddit API deviations found by testing against the live server**, all handled in
`pkg/redditchat`:

| Behaviour | Handling |
| --- | --- |
| `GET /rooms/{id}/state` returns **403** | Member list comes from `/members`; name, topic and avatar are fetched as individual state events, each tolerating a 404 |
| `t0_0` is the **end**-of-history marker, not a starting cursor — paginating from it returns empty chunks forever | Backward pagination starts with `from` omitted; `t0_0` terminates the walk (`HasMoreHistory`) |
| Most chats have no `m.room.name`/`topic`/`avatar` at all | A 404 for those is normal, not an error |
| Reddit tags rooms with a custom `com.reddit.chat.type` event | Ignored; DM detection uses `is_direct` and the member count |
| Replies are **threads**: Reddit sends `rel_type: m.thread` with an `m.in_reply_to` fallback, never a plain reply | A Matrix reply or thread reply both map onto a Reddit thread, keyed by the stored Reddit event ID |
| Reactions never appear in `/messages`, only in `/sync` | They cannot be backfilled, only observed live |
| Reaction keys are Reddit emoji IDs (e.g. `jvuspmbga7081.gif`, served from `i.redd.it`), and unicode is refused with `M_INVALID_ARGUMENT_VALUE: reaction key is not supported` | Reactions are advertised as unsupported rather than failing after the user sends one |
| `/sync` caps its room list at **20 joined + 20 invited**, regardless of filter, timeline limit or `full_state` | `/joined_rooms` is not capped, so the bridge reconciles against it once per connection and bridges whatever sync omitted. On the test account sync showed 20 while `/joined_rooms` showed 23 |
| In an unaccepted chat, `/members` and `/state/*` return **403** ("has never joined this room") while `/messages` works | Chat info for those portals is built from sync's `invite_state`, which carries the member events and display names; `getChatInfo` treats a 403 as "still pending" rather than an error |

## Layout

| Path | What's in it |
| --- | --- |
| `cmd/matrix-redditchat` | `main()` and the double puppet registration generator |
| `pkg/redditchat` | Reddit CS-API client; all Reddit quirks live here |
| `pkg/connector` | The bridgev2 network connector: login, sync, chat info, sending, backfill |
