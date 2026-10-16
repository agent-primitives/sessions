// Shared plumbing for the landing page's looping figures: a timeline that can pause and resume
// where it left off, and the pause button every looping figure carries (WCAG 2.2.2).

export const prefersReducedMotion = () =>
  window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/**
 * A schedule of callbacks on one clock that can be paused and resumed mid-step. Plain setTimeout
 * chains cannot pause: clearing them loses the position, so a paused figure would restart.
 */
export function timeline() {
  let tasks: { t: number; fn: () => void }[] = [];
  let elapsed = 0;
  let startedAt = 0;
  let running = false;
  let timer = 0;

  const clock = () => elapsed + (running ? performance.now() - startedAt : 0);

  const schedule = () => {
    window.clearTimeout(timer);
    if (!running || tasks.length === 0) return;
    tasks.sort((a, b) => a.t - b.t);
    timer = window.setTimeout(fire, Math.max(0, tasks[0].t - clock()));
  };

  function fire() {
    // A callback may reset the clock and schedule a new loop, so re-read it on every pass.
    while (running && tasks.length > 0 && tasks[0].t <= clock()) {
      tasks.shift()!.fn();
      tasks.sort((a, b) => a.t - b.t);
    }
    schedule();
  }

  return {
    /** Runs `fn` at `t` ms on this timeline's clock. */
    at(t: number, fn: () => void) {
      tasks.push({ t, fn });
      schedule();
    },
    /** Runs `fn` `ms` from now on this timeline's clock. */
    after(ms: number, fn: () => void) {
      this.at(clock() + ms, fn);
    },
    /** Drops every pending callback and rewinds the clock to zero. */
    reset() {
      tasks = [];
      elapsed = 0;
      startedAt = performance.now();
      schedule();
    },
    pause() {
      if (!running) return;
      elapsed = clock();
      running = false;
      window.clearTimeout(timer);
    },
    resume() {
      if (running) return;
      startedAt = performance.now();
      running = true;
      schedule();
    },
  };
}

/**
 * Drives a looping figure: plays while it is on screen, stops off screen, and honours the pause
 * button in its chrome bar. `start` restarts the loop from the top; `pause`/`resume` hold and
 * continue it. A user pause is sticky: scrolling away and back does not restart the figure.
 */
export function loopControl(
  root: HTMLElement,
  opts: { threshold: number; start: () => void; pause: () => void; resume: () => void },
) {
  const button = root.querySelector<HTMLButtonElement>("[data-motion-toggle]");
  let userPaused = false;
  let visible = false;
  let started = false;

  const setPaused = (paused: boolean) => {
    root.classList.toggle("is-paused", paused);
    button?.setAttribute("aria-pressed", String(paused));
  };

  const sync = () => {
    const play = visible && !userPaused;
    if (play && !started) {
      started = true;
      opts.resume();
      opts.start();
    } else if (play) {
      opts.resume();
    } else {
      opts.pause();
    }
  };

  if (button) {
    button.hidden = false;
    button.addEventListener("click", () => {
      userPaused = !userPaused;
      setPaused(userPaused);
      sync();
    });
  }

  new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        visible = e.isIntersecting;
        // Leaving the screen rewinds an unpaused figure, so it is at the top when it comes back.
        if (!visible && !userPaused) started = false;
        sync();
      }
    },
    { threshold: opts.threshold },
  ).observe(root);
}
