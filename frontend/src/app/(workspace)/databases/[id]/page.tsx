import { DatabaseViewer } from "@/components/database-viewer";

export default async function DatabasePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <DatabaseViewer id={id} />;
}
