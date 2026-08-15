-- Blueprint v1 runtime defaults: four configured search sources and deep baseline.
UPDATE providers
SET enabled = CASE WHEN name IN ('brave', 'tavily', 'exa', 'grok') THEN enabled ELSE FALSE END,
    updated_at = now()
WHERE name IN ('you', 'jina', 'firecrawl', 'serper');

UPDATE settings
SET value = jsonb_set(
          jsonb_set(value, '{default_mode}', '"deep"'::jsonb, true),
          '{default_providers}', '["brave","grok"]'::jsonb, true
      ),
    updated_at = now()
WHERE key = 'runtime';

INSERT INTO settings (key, value)
VALUES ('blueprint_v1_defaults_applied', 'true'::jsonb)
ON CONFLICT (key) DO NOTHING;
