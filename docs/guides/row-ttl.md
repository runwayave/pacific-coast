# Row-level TTL

Declare a `ttl_field` on any entity with a `timestamptz not null` column, and atlantis automatically deletes expired rows on a 1-minute sweep.

## 1. Add the TTL column + directive

```atl
entity Session in consumer {
  id            varchar(8) primary
  consumer_id   varchar(8) not null references consumer.Account.id
  session_token varchar(255) not null unique
  expires_at    timestamptz not null
  created_at    timestamptz not null default now()

  ttl_field expires_at
}
```

`ttl_field` names the column the sweeper checks. The column must be `timestamptz` and `not null`.

## 2. Apply

```bash
tide apply
```

The `ttl_field` directive is recorded in the IR checkpoint. The built-in `SweepExpired` job reads the checkpoint at runtime to discover which entities have TTL columns.

## 3. Verify

Insert a row with `expires_at` in the past:

```sql
INSERT INTO consumer.sessions (id, consumer_id, session_token, expires_at)
VALUES ('s1', 'c1', 'tok', now() - interval '1 hour');
```

Within a minute the sweeper deletes it. Check:

```bash
tide job status <sweeper-job-id>
```

## How it works

- atlantis ships a built-in job `atlantis.SweepExpired` that runs on a `* * * * *` cron schedule (every minute).
- On each fire, the sweeper loads the IR checkpoint, finds every entity with `ttl_field` set, and runs `DELETE FROM <table> WHERE <ttl_field> < now() LIMIT 1000`.
- The batch limit prevents vacuum churn; leftover rows get caught on the next sweep tick.
- Operators can tune the cadence by updating `atlantis.job_schedules` directly (`UPDATE ... SET cron_spec = '*/5 * * * *'`) or disable with `enabled = false`.

## Related

- [Jobs and workflows concept](../concepts/jobs-and-workflows.md). The sweeper is itself a job running on the atlantis runtime.
- [Declarative jobs guide](declarative-jobs.md). How to declare and run your own jobs.
