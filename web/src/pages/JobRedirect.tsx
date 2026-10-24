import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { get } from "../api";
import { ErrorBox, Spinner } from "../components/ui";
import type { Job } from "../types";

/** /jobs/:jobId -> /runs/:runId/jobs/:jobId */
export function JobRedirect() {
  const { jobId } = useParams();
  const navigate = useNavigate();
  const [error, setError] = useState(false);
  useEffect(() => {
    get<Job>(`/api/jobs/${jobId}`)
      .then((job) => navigate(`/runs/${job.run_id}/jobs/${job.id}`, { replace: true }))
      .catch(() => setError(true));
  }, [jobId, navigate]);
  return (
    <div className="page">
      {error ? (
        <ErrorBox>
          Job {jobId} not found. <Link to="/">Back to runs</Link>
        </ErrorBox>
      ) : (
        <Spinner label="Finding job…" />
      )}
    </div>
  );
}
