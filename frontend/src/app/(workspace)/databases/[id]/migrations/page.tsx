import { MigrationWorkspace } from "@/components/migration-workspace";

export default async function MigrationsPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <MigrationWorkspace id={id} />;
}
