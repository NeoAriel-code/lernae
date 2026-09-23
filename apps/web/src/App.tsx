export default function App() {
  return (
    <main className="page-shell">
      <header className="brand">
        <p className="eyebrow">One library. Your universe.</p>
        <h1>Lernae</h1>
      </header>

      <section className="foundation" aria-labelledby="foundation-title">
        <p className="eyebrow">Phase 0</p>
        <h2 id="foundation-title">Foundation scaffold</h2>
        <p>
          This application shell is not yet connected to the Server, database,
          or Agent. Runtime integration is deferred to Phase 1.
        </p>
      </section>

      <footer>Self-hosted foundation · React + TypeScript</footer>
    </main>
  );
}
