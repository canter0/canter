"use client";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { ResourceRequest, type ResourceKind } from "@/components/resource-request";

export function ResourceExamples() {
  const router = useRouter();
  const [kind, setKind] = useState<ResourceKind>("compute");
  const [message, setMessage] = useState("");
  return <div style={{ background: "#141414", color: "#e3e3e3", minHeight: "100vh", colorScheme: "dark" }}><main style={{ maxWidth: 650, padding: "64px 24px", margin: "auto" }}>
    <p style={{ color: "#aaa", fontSize: 12, marginBottom: 24 }}>Canter · Development UI examples · Nothing is provisioned</p>
    <h1 style={{ fontSize: 26, marginBottom: 24 }}>Ask, then choose.</h1>
    <nav aria-label="Examples" style={{ display: "flex", gap: 24, fontSize: 14 }}><button onClick={() => { setKind("compute"); setMessage(""); }} aria-pressed={kind === "compute"}>VPS</button><button onClick={() => { setKind("storage"); setMessage(""); }} aria-pressed={kind === "storage"}>Storage bucket</button><a href="/app">GitHub in Canter ↗</a></nav>
    <p style={{ marginTop: 36, fontSize: 14 }}>{kind === "compute" ? "“I want to make a VPS.”" : "“I want to create a storage bucket.”"}</p>
    <ResourceRequest key={kind} kind={kind} busy={false} onContinue={async text => { setMessage(text); return true; }} onGitHub={() => router.push("/app")} />
    {message ? <section aria-label="Example request"><h2 style={{ fontSize: 14 }}>Example message sent by this form</h2><p style={{ color: "#aaa", fontSize: 13, lineHeight: 1.8, marginTop: 12 }}>{message}</p><p style={{ color: "#aaa", fontSize: 12, marginTop: 12 }}>Preview only. In the workspace, this message goes to your agent.</p></section> : null}
  </main></div>;
}
