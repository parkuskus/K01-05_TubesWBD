import { OperationWorkspace } from "@/components/operation-workspace";

export default async function OperationPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <OperationWorkspace key={id} id={id} />;
}
