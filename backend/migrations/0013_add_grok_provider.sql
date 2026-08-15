INSERT INTO providers (name, display_name, base_url, priority, weight, timeout_ms, default_cache_enabled, cache_ttl_seconds, settings)
VALUES
    ('grok', 'Grok (AI Search)', 'http://127.0.0.1:8000', 80, 1, 30000, FALSE, 3600, '{"key_retry_count":1,"max_concurrency":0}'::jsonb)
ON CONFLICT (name) DO NOTHING;

UPDATE settings
SET value = jsonb_set(value, '{default_providers}', '["exa","you","jina","tavily","firecrawl","serper","brave","grok"]'::jsonb, true),
    updated_at = now()
WHERE key = 'runtime'
  AND COALESCE(value->'default_providers', '[]'::jsonb) = '["exa","you","jina","tavily","firecrawl","serper","brave"]'::jsonb;
