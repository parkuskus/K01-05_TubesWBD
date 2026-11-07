ALTER TABLE groups ADD COLUMN leader_user_id BIGINT;

ALTER TABLE groups ADD CONSTRAINT groups_leader_membership_fk
    FOREIGN KEY (id, leader_user_id) REFERENCES group_memberships(group_id, user_id)
    DEFERRABLE INITIALLY IMMEDIATE;

CREATE TABLE platform_policy_version (
    id BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    version BIGINT NOT NULL CHECK (version > 0)
);
INSERT INTO platform_policy_version (id, version) VALUES (true, 1);

CREATE FUNCTION advance_platform_policy_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE platform_policy_version SET version = version + 1 WHERE id = true;
    RETURN NULL;
END;
$$;

CREATE TRIGGER membership_policy_insert AFTER INSERT ON group_memberships
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER membership_policy_delete AFTER DELETE ON group_memberships
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER grant_policy_insert AFTER INSERT ON group_database_access
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER grant_policy_delete AFTER DELETE ON group_database_access
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER grant_policy_update AFTER UPDATE OF level ON group_database_access
    FOR EACH ROW WHEN (OLD.level IS DISTINCT FROM NEW.level)
    EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER leader_policy_update AFTER UPDATE OF leader_user_id ON groups
    FOR EACH ROW WHEN (OLD.leader_user_id IS DISTINCT FROM NEW.leader_user_id)
    EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER connection_policy_insert AFTER INSERT ON database_connections
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER connection_policy_delete AFTER DELETE ON database_connections
    FOR EACH ROW EXECUTE FUNCTION advance_platform_policy_version();
CREATE TRIGGER connection_policy_update AFTER UPDATE ON database_connections
    FOR EACH ROW WHEN (
        (OLD.name, OLD.host, OLD.port, OLD.database_name, OLD.username, OLD.ssl_mode,
         OLD.password_ciphertext, OLD.revision)
        IS DISTINCT FROM
        (NEW.name, NEW.host, NEW.port, NEW.database_name, NEW.username, NEW.ssl_mode,
         NEW.password_ciphertext, NEW.revision)
    )
    EXECUTE FUNCTION advance_platform_policy_version();
