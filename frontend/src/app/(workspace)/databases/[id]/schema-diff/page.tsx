import { SchemaDiffWorkspace } from "@/components/schema-diff-workspace";

export default async function SchemaDiffPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <SchemaDiffWorkspace id={id} />;
}
