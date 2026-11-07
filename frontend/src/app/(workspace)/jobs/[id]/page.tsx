import { JobWorkspace } from "@/components/job-workspace";

export default async function JobPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <JobWorkspace id={id} />;
}
