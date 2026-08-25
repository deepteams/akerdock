-- Omni serving is a modality of the engine, not another engine (ADR-083).
-- The engine enum keeps its two values because an engine IS a flag
-- vocabulary — vLLM-Omni is a plugin of vLLM and spells every knob the vLLM
-- way, SGLang-Omni spells them the SGLang way. What changes is the
-- invocation, resolved from the pair (engine, modality) by the renderer.
--
-- An omni runtime has no image the platform can pin (neither project
-- publishes one): the override is required, and the CHECK says so in the
-- place no code path can talk its way around.

-- +goose Up
CREATE TYPE inference_modality AS ENUM ('text', 'omni');

ALTER TABLE models ADD COLUMN modality inference_modality NOT NULL DEFAULT 'text';

ALTER TABLE models ADD CONSTRAINT models_omni_requires_image CHECK (
    modality = 'text' OR (image IS NOT NULL AND image <> '')
);

-- +goose Down
ALTER TABLE models DROP CONSTRAINT models_omni_requires_image;
ALTER TABLE models DROP COLUMN modality;
DROP TYPE inference_modality;
