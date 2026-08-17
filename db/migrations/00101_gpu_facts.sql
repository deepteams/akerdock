-- The GPU is an observed fact of a server (ADR-079), recorded by validation
-- exactly like the OS, the architecture and the Docker version — never a
-- checkbox an operator ticks: a checkbox can lie, `nvidia-smi` cannot. NULL
-- means "none observed", which is also every existing server's answer until
-- its next validation. gpu_memory_mb is what the driver reports (a unified
-- memory machine reports the shared pool): information for the operator and
-- the placement guard, not an allocator.

-- +goose Up
ALTER TABLE servers
    ADD COLUMN gpu_name text,
    ADD COLUMN gpu_memory_mb integer;

-- +goose Down
ALTER TABLE servers DROP COLUMN gpu_name, DROP COLUMN gpu_memory_mb;
