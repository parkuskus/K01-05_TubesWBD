import { MigrationEditor } from "@/components/migration-editor";

export default async function NewMigrationPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <MigrationEditor id={id} />;
}
