import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { errorMessage, post } from "../api";
import type { Job, Run } from "../types";
import { ErrorBox, Modal } from "./ui";

/** Which jobs a re-run runs again: all of them, the failed ones, or one. */
export type RerunWhat = { kind: "all" } | { kind: "failed" } | { kind: "job"; job: Job };

/** Confirms a re-run of a run's jobs and opens the new attempt. */
export function RerunModal({ run, what, onClose }: { run: Run; what: RerunWhat; onClose: () => void }) {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const title = what.kind === "all" ? "Re-run all jobs" : what.kind === "failed" ? "Re-run failed jobs" : `Re-run ${what.job.name}`;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    const body = what.kind === "failed" ? { failed: true } : what.kind === "job" ? { job_id: what.job.id } : {};
    try {
      const res = await post<{ run_id: number }>(`/api/runs/${run.id}/rerun`, body);
      onClose();
      navigate(`/runs/${res.run_id}`);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={title} onClose={onClose}>
      <form onSubmit={submit} className="form">
        {error && <ErrorBox>{error}</ErrorBox>}
        <p className="muted">
          {what.kind === "all"
            ? "Every job of this run runs again, in a new attempt."
            : what.kind === "failed"
              ? "The jobs that failed or were cancelled run again in a new attempt, with the jobs that need them. The other jobs keep their results, outputs and logs."
              : "This job runs again in a new attempt, with the jobs that need it. The other jobs keep their results, outputs and logs."}
        </p>
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy} autoFocus>
            {busy ? "Starting…" : title}
          </button>
        </div>
      </form>
    </Modal>
  );
}
