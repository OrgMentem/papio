-- The durable reason one human action exists. Before this column the inbox
-- had to reconstruct it from a browser.provider_outcome event join, which no
-- producer ever wrote and which two of the five manual-download producers do
-- not emit at all, so every reason collapsed into one task family. The value
-- is a closed job.DiagnosisReason; NULL means a row predating this column or a
-- producer with no structured reason, and must keep rendering as a plain
-- manual download rather than being guessed from its prose detail.
ALTER TABLE human_actions ADD COLUMN diagnosis TEXT;
