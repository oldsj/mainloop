-- +goose Up
-- The version of the shared agent guidance (agentcontext.Version) a main binding's Session was last
-- given, and the delivery that gave it, so a delivery that kagent never saw can give it back. Both
-- are NULL until a context block is delivered; a Session that has the standing context but not the
-- current guidance version gets the context block alone on its next message.
ALTER TABLE agent_bindings
    ADD COLUMN guidance_version integer,
    ADD COLUMN guidance_delivery_id text,
    ADD CONSTRAINT agent_bindings_guidance_given CHECK ((guidance_version IS NULL) = (guidance_delivery_id IS NULL));

-- +goose Down
ALTER TABLE agent_bindings
    DROP CONSTRAINT agent_bindings_guidance_given,
    DROP COLUMN guidance_delivery_id,
    DROP COLUMN guidance_version;
