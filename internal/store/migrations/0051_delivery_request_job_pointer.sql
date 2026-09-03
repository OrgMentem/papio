-- Pin each routed job to its delivery request. Older databases can contain
-- several historical rows for a job, so retain the most recently created row.
UPDATE jobs
SET delivery_request_id = (
    SELECT id
    FROM delivery_requests
    WHERE delivery_requests.job_id = jobs.id
    ORDER BY id DESC
    LIMIT 1
)
WHERE delivery_request_id IS NULL
  AND EXISTS (
      SELECT 1
      FROM delivery_requests
      WHERE delivery_requests.job_id = jobs.id
  );

CREATE INDEX IF NOT EXISTS jobs_delivery_request_id_idx
    ON jobs(delivery_request_id);
