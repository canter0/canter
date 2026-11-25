import type { WorkspaceIconName } from "@/components/workspace-icon";
import { dashboardRequestPrompts } from "./dashboard-request-prompts";

type ProductSection = { title: string; description: string };
type PublicProduct = {
  path: string;
  label: string;
  menuDescription: string;
  icon: WorkspaceIconName;
  hero: ProductSection;
  sections: readonly [ProductSection, ProductSection];
  prompt: string;
};

export const publicProducts = {
  computers: {
    path: "/sandboxes/computers",
    label: "Computers",
    menuDescription: "Explore cloud computers",
    icon: "desktop",
    hero: {
      title: "A cloud computer for your agent.",
      description: "Open a sandbox for files, commands, and tests.",
    },
    sections: [
      { title: "Work with your files.", description: "Browse files and run commands from the same sandbox." },
      { title: "Sleep it. Wake it later.", description: "Return to your saved environment when the sandbox wakes." },
    ],
    prompt: dashboardRequestPrompts.createSandbox,
  },
  "super-sandboxes": {
    path: "/sandboxes/super",
    label: "Super Sandboxes",
    menuDescription: "Run batches across sandboxes",
    icon: "sandboxCollection",
    hero: {
      title: "Run a batch across sandboxes.",
      description: "Send independent tasks to sandbox workers.",
    },
    sections: [
      { title: "See each task’s outcome.", description: "Check which worker ran each task and how it finished." },
      { title: "Go from a task to its logs.", description: "Open the worker’s Jobs view using the task’s job ID." },
    ],
    prompt: "Help me plan a Super Sandbox batch. Ask what tasks I want to run, then help me choose workers and concurrency before starting the batch.",
  },
  "saved-environments": {
    path: "/sandboxes/saved-environments",
    label: "Saved environments",
    menuDescription: "Save and reuse a sandbox setup",
    icon: "bookmark",
    hero: {
      title: "Keep a setup you can use again.",
      description: "Save a sandbox environment and reuse it later.",
    },
    sections: [
      { title: "Save when a sandbox sleeps.", description: "When you wake the sandbox, it can resume from its save." },
      { title: "Start from a template.", description: "Use a saved setup to create another sandbox. Extra kept saves and templates require billing." },
    ],
    prompt: dashboardRequestPrompts.savedEnvironments,
  },
  vps: {
    path: "/vps",
    label: "VPS overview",
    menuDescription: "See how app hosting works",
    icon: "serverStack",
    hero: {
      title: "Run your app on a VPS.",
      description: "Host your app, database, and public HTTPS endpoint.",
    },
    sections: [
      { title: "Approve the deployment plan.", description: "Check the proposed changes and hosting costs before deploying." },
      { title: "Check in after launch.", description: "Review health checks and logs, or roll back to an earlier release." },
    ],
    prompt: dashboardRequestPrompts.deployApp,
  },
  apps: {
    path: "/vps/apps",
    label: "Your apps",
    menuDescription: "See how to manage deployed apps",
    icon: "apps",
    hero: {
      title: "See what you’re running.",
      description: "Open a deployed app and inspect its resources.",
    },
    sections: [
      { title: "Find what belongs to an app.", description: "See the web service, database, and public endpoint together." },
      { title: "Check the latest release.", description: "Open release details to see what’s live and inspect health and logs." },
    ],
    prompt: "Help me review the apps deployed in this organisation. List what is running and help me inspect the resources and latest release of the app I choose.",
  },
  ci: {
    path: "/ci",
    label: "CI overview",
    menuDescription: "Explore GitHub Actions runners",
    icon: "pipeline",
    hero: {
      title: "Run GitHub Actions on Canter.",
      description: "Use Canter-managed runners for your workflow jobs.",
    },
    sections: [
      { title: "Choose a runner for your job.", description: "Copy the runner label from the CI workspace and use it in your workflow." },
      { title: "Connect GitHub and run a workflow.", description: "An organisation owner connects GitHub so jobs can run on Canter." },
    ],
    prompt: "Help me set up a Canter runner for GitHub Actions. Ask which repository and workflow I want to use, then help me choose a runner and update the workflow.",
  },
  "ci-workspace": {
    path: "/ci/workspace",
    label: "CI workspace",
    menuDescription: "See workflow jobs and runners",
    icon: "changes",
    hero: {
      title: "See which jobs ran.",
      description: "Find GitHub Actions jobs and check their results.",
    },
    sections: [
      { title: "Find a job by name.", description: "Filter recent jobs and open a job in GitHub." },
      { title: "Check your runner pool.", description: "See pool status and copy labels for available runner sizes." },
    ],
    prompt: "Help me inspect CI in this organisation. Show me how to find recent GitHub Actions jobs and check the runner pool.",
  },
} as const satisfies Record<string, PublicProduct>;

export type PublicProductId = keyof typeof publicProducts;

export function isPublicProductId(id: string): id is PublicProductId {
  return Object.hasOwn(publicProducts, id);
}
