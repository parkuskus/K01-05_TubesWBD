import { MigrationEditor } from "@/components/migration-editor";

export default async function MigrationPage({
  params,
}: {
  params: Promise<{ id: string; migrationId: string }>;
}) {
  const { id, migrationId } = await params;
  return <MigrationEditor id={id} migrationId={migrationId} />;
}
