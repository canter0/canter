export const taskModels = [
  { id: "gpt-5.6-luna", label: "GPT 5.6 Luna" },
  { id: "deepseek-v4.1-flash", label: "DeepSeek V4.1 Flash" },
  { id: "glm-5.3-flash", label: "GLM 5.3 Flash" },
] as const;
export const reasoningLevels = ["low", "medium", "high"] as const;
export const taskStatus: Record<string, string> = { queued: "Waiting for an agent", working: "In progress", completed: "Completed", failed: "Failed" };
