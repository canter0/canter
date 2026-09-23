import { notFound } from "next/navigation";
import { ResourceExamples } from "./resource-examples";

export default function ResourceExamplesPage() {
  if (process.env.NODE_ENV !== "development") notFound();
  return <ResourceExamples />;
}
