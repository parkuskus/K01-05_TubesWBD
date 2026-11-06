import { GroupWorkspace } from "@/components/groups-workspace";

export default async function GroupPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <GroupWorkspace id={id} />;
}
