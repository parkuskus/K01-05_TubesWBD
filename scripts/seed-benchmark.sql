-- Deterministic server-side dataset generator for Milestone 2 batch migration benchmark (M2-K09).
-- Uses PostgreSQL generate_series(1, N) so no rows are materialized in application memory.
-- Does NOT use pg_sleep or artificial timers.

DROP TABLE IF EXISTS public.bench_orders;

CREATE TABLE public.bench_orders (
    id BIGINT PRIMARY KEY,
    customer_code TEXT NOT NULL,
    amount_cents BIGINT NOT NULL,
    status TEXT NOT NULL,
    loyalty_tier TEXT NOT NULL DEFAULT 'STANDARD',
    updated_flag BOOLEAN NOT NULL DEFAULT FALSE
);

INSERT INTO public.bench_orders (
    id,
    customer_code,
    amount_cents,
    status,
    loyalty_tier,
    updated_flag
)
SELECT
    gs AS id,
    'CUST-' || lpad(((gs % 100000) + 1)::text, 6, '0') AS customer_code,
    ((gs * 37) % 500000) + 1000 AS amount_cents,
    CASE
        WHEN gs % 10 = 0 THEN 'CANCELLED'
        ELSE 'SETTLED'
    END AS status,
    'STANDARD' AS loyalty_tier,
    FALSE AS updated_flag
FROM generate_series(1, 1000000) AS gs;

CREATE INDEX bench_orders_status_id_idx
    ON public.bench_orders (status, id);

CREATE INDEX bench_orders_loyalty_tier_idx
    ON public.bench_orders (loyalty_tier, customer_code);

VACUUM ANALYZE public.bench_orders;
