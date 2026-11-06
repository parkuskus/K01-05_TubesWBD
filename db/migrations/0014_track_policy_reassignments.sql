CREATE TRIGGER membership_policy_update AFTER UPDATE ON group_memberships
    FOR EACH ROW WHEN (
        (OLD.group_id, OLD.user_id) IS DISTINCT FROM (NEW.group_id, NEW.user_id)
    ) EXECUTE FUNCTION advance_platform_policy_version();

DROP TRIGGER grant_policy_update ON group_database_access;
CREATE TRIGGER grant_policy_update AFTER UPDATE ON group_database_access
    FOR EACH ROW WHEN (
        (OLD.group_id, OLD.database_id, OLD.level)
        IS DISTINCT FROM (NEW.group_id, NEW.database_id, NEW.level)
    ) EXECUTE FUNCTION advance_platform_policy_version();
