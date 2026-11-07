import { HistoryWorkspace } from "@/components/history-workspace";

export default async function HistoryPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <HistoryWorkspace id={id} />;
}
