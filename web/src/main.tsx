import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { ProjectsPage } from "./projects";
import { ProjectRunsPage } from "./project-runs";
import { NewRunPage } from "./new-run";
import { RunPage } from "./run";
import "./style.css";
export type Navigate = (path: string) => void;
function App() {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const pop = () => setPath(location.pathname);
    window.addEventListener("popstate", pop);
    return () => window.removeEventListener("popstate", pop);
  }, []);
  const navigate: Navigate = (next) => {
    history.pushState(null, "", next);
    setPath(next);
    window.scrollTo(0, 0);
  };
  const parts = path.split("/").filter(Boolean);
  let page: React.ReactNode;
  if (parts.length === 0 || (parts[0] === "projects" && parts.length === 1))
    page = <ProjectsPage navigate={navigate} />;
  else if (parts[0] === "projects" && parts[1] && parts.length === 2)
    page = <ProjectRunsPage projectID={parts[1]} navigate={navigate} />;
  else if (
    parts[0] === "projects" &&
    parts[1] &&
    parts[2] === "new-run" &&
    parts.length === 3
  )
    page = <NewRunPage projectID={parts[1]} navigate={navigate} />;
  else if (parts[0] === "runs" && parts[1] && parts.length === 2)
    page = <RunPage runID={parts[1]} navigate={navigate} />;
  else
    page = (
      <main className="not-found">
        <h1>Page not found</h1>
        <a
          href="/projects"
          onClick={(event) => {
            event.preventDefault();
            navigate("/projects");
          }}
        >
          Projects
        </a>
      </main>
    );
  return page;
}
createRoot(document.getElementById("root")!).render(<App />);
