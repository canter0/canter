import { canterFetch } from "./canter-api";
import { createRepositoryInspectionCache, type RepositoryInspection } from "./repository-inspection-cache";

export const inspectRepository = createRepositoryInspectionCache(path => canterFetch<RepositoryInspection>(path));
export type { RepositoryInspection } from "./repository-inspection-cache";
