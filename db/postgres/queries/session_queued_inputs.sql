-- name: LockSessionQueue :exec
-- Serializes writers of one session queue for the length of the transaction, so
-- the capacity count and the insert that follows it see the same queue.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: InsertSessionQueuedInput :one
-- Returns no row when the invocation already has one; the caller reads that row
-- and decides between a replay and a conflict.
INSERT INTO session_queued_inputs (
  id,
  bot_id,
  session_id,
  origin,
  invocation_id,
  merge_key,
  payload,
  enqueued_during_run_id,
  received_at,
  created_at
) VALUES (
  sqlc.arg(id),
  sqlc.arg(bot_id),
  sqlc.arg(session_id),
  sqlc.arg(origin),
  sqlc.arg(invocation_id),
  sqlc.arg(merge_key),
  sqlc.arg(payload),
  sqlc.arg(enqueued_during_run_id),
  sqlc.narg(received_at)::timestamptz,
  COALESCE(sqlc.narg(created_at)::timestamptz, now())
)
ON CONFLICT (team_id, session_id, invocation_id) DO NOTHING
RETURNING *;

-- name: GetSessionQueuedInputByInvocation :one
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND invocation_id = sqlc.arg(invocation_id);

-- name: GetSessionQueuedInput :one
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND id = sqlc.arg(id);

-- name: CountQueuedSessionInputs :one
SELECT count(*)::bigint
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued';

-- name: QueuedInputGate :one
-- What admitting a new turn on a session has to know about its queue: whether
-- the turn's own invocation is already queued or consumed, and whether anything
-- is still waiting ahead of it.
SELECT
  (sqlc.arg(invocation_id)::text <> '' AND EXISTS (
    SELECT 1
    FROM session_queued_inputs AS seen
    WHERE seen.team_id = public.memoh_current_team_id()
      AND seen.session_id = sqlc.arg(session_id)
      AND seen.invocation_id = sqlc.arg(invocation_id)
  ))::boolean AS seen,
  EXISTS (
    SELECT 1
    FROM session_queued_inputs AS waiting
    WHERE waiting.team_id = public.memoh_current_team_id()
      AND waiting.session_id = sqlc.arg(session_id)
      AND waiting.state IN ('queued', 'claimed')
  )::boolean AS waiting;

-- name: ListSessionQueuedInputs :many
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued'
ORDER BY position, created_at, id;

-- name: ListSessionQueuedInputsForUpdate :many
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued'
ORDER BY position, created_at, id
FOR UPDATE;

-- name: SetSessionQueuedInputPosition :exec
UPDATE session_queued_inputs
SET position = sqlc.arg(position),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND state = 'queued';

-- name: UpdateSessionQueuedInputPayload :one
UPDATE session_queued_inputs
SET payload = sqlc.arg(payload),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND id = sqlc.arg(id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued'
RETURNING *;

-- name: CancelSessionQueuedInput :execrows
UPDATE session_queued_inputs
SET state = 'cancelled',
    settled_at = now(),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND id = sqlc.arg(id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued';

-- name: ListQueuedInputsToClaim :many
-- SKIP LOCKED keeps two drainers from waiting on each other: the one that finds
-- the rows locked has nothing to claim.
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND origin = sqlc.arg(origin)
  AND state = 'queued'
ORDER BY position, created_at, id
FOR UPDATE SKIP LOCKED;

-- name: ListLapsedQueuedInputs :many
-- Claims whose lease ran out: the batch a drainer took and then neither
-- settled nor released.
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND origin = sqlc.arg(origin)
  AND state = 'claimed'
  AND claimed_until < clock_timestamp()
ORDER BY position, created_at, id
FOR UPDATE SKIP LOCKED;

-- name: HasLiveQueuedClaim :one
SELECT EXISTS (
  SELECT 1
  FROM session_queued_inputs
  WHERE team_id = public.memoh_current_team_id()
    AND session_id = sqlc.arg(session_id)
    AND origin = sqlc.arg(origin)
    AND state = 'claimed'
    AND claimed_until >= clock_timestamp()
)::boolean AS live;

-- name: ClaimQueuedInputs :many
UPDATE session_queued_inputs
SET state = 'claimed',
    batch_id = sqlc.arg(batch_id),
    claim_token = sqlc.arg(claim_token),
    claimed_until = clock_timestamp() + make_interval(secs => sqlc.arg(lease_seconds)::double precision),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = ANY(sqlc.arg(ids)::uuid[])
  AND state = 'queued'
RETURNING *;

-- name: RenewQueuedBatchClaim :many
UPDATE session_queued_inputs
SET claim_token = sqlc.arg(claim_token),
    claimed_until = clock_timestamp() + make_interval(secs => sqlc.arg(lease_seconds)::double precision),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND batch_id = sqlc.arg(batch_id)
  AND state = 'claimed'
RETURNING *;

-- name: SettleQueuedBatch :execrows
-- The batch's run exists, whether this call admitted it or found it admitted.
UPDATE session_queued_inputs
SET state = 'consumed',
    run_invocation_id = 'queued:' || batch_id::text,
    settled_at = now(),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND batch_id = sqlc.arg(batch_id)
  AND state = 'claimed';

-- name: ReleaseQueuedBatch :execrows
UPDATE session_queued_inputs
SET state = 'queued',
    batch_id = NULL,
    claim_token = NULL,
    claimed_until = NULL,
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND batch_id = sqlc.arg(batch_id)
  AND claim_token = sqlc.arg(claim_token)
  AND state = 'claimed';

-- name: RejectQueuedInput :execrows
-- For an input no drainer can ever start. Retrying it would fail the same way
-- and block everything behind it.
UPDATE session_queued_inputs
SET state = 'cancelled',
    error_code = sqlc.arg(error_code),
    settled_at = now(),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND state IN ('queued', 'claimed');

-- name: ListDrainableQueuedSessions :many
SELECT bot_id, session_id
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND origin = sqlc.arg(origin)
  AND (state = 'queued' OR (state = 'claimed' AND claimed_until < clock_timestamp()))
GROUP BY bot_id, session_id
ORDER BY min(created_at), session_id
LIMIT sqlc.arg(max_sessions);

-- name: BeginQueuedInputPromotion :one
UPDATE session_queued_inputs
SET state = 'promoting',
    claim_token = gen_random_uuid(),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND id = sqlc.arg(id)
  AND origin = 'follow_up'
  AND state = 'queued'
RETURNING *;

-- name: CompleteQueuedInputPromotion :execrows
UPDATE session_queued_inputs
SET state = 'promoted',
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND claim_token = sqlc.arg(claim_token)
  AND state = 'promoting';

-- name: AbortQueuedInputPromotion :execrows
UPDATE session_queued_inputs
SET state = 'queued',
    claim_token = NULL,
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND claim_token = sqlc.arg(claim_token)
  AND state = 'promoting';

-- name: ConsumePromotedQueuedInput :execrows
UPDATE session_queued_inputs
SET state = 'consumed',
    settled_at = now(),
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND state IN ('promoting', 'promoted');

-- name: RequeuePromotedQueuedInput :execrows
UPDATE session_queued_inputs
SET state = 'queued',
    claim_token = NULL,
    updated_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND state IN ('promoting', 'promoted');

-- name: ListSessionQueuedPromotions :many
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND state IN ('promoting', 'promoted')
ORDER BY position;

-- name: ListStaleQueuedPromotions :many
SELECT *
FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND state IN ('promoting', 'promoted')
  AND updated_at < sqlc.arg(before)::timestamptz
ORDER BY updated_at
LIMIT sqlc.arg(max_items);

-- name: PurgeSettledQueuedInputs :execrows
DELETE FROM session_queued_inputs
WHERE team_id = public.memoh_current_team_id()
  AND state IN ('consumed', 'cancelled')
  AND settled_at < sqlc.arg(before)::timestamptz;
