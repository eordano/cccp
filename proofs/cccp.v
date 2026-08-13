(* ===================================================================== *)
(* cccp.v -- a machine-checkable model of cccp's sync design.             *)
(*                                                                       *)
(* Models what the Go CODE does (internal/api/ingest.go, log.go,         *)
(* internal/store/store.go, internal/agent/pull.go, push.go, walk.go,    *)
(* internal/secretary/serializer.go), not what the README promises.      *)
(* Parts 1-5 prove the server-side stores sound.  Parts 6-7 are honesty  *)
(* lemmas: machine-checked counterexamples showing which preconditions   *)
(* of Parts 4-5 the CLIENT code fails to establish, and which client     *)
(* behaviors the positive theorems do not endorse.  See proofs/README.md *)
(* for the line-by-line map and the operational discipline that follows. *)
(*                                                                       *)
(* No Admitted, no Axiom, no external libraries -- stdlib only.           *)
(* ===================================================================== *)

From Stdlib Require Import List.
From Stdlib Require Import Arith.
From Stdlib Require Import Bool.
From Stdlib Require Import Lia.
Import ListNotations.

(* ===================================================================== *)
(* Part 0. Generic helper: the last element of a list, if any.           *)
(* Models store.go LatestVersion: ORDER BY version DESC LIMIT 1.         *)
(* ===================================================================== *)

Fixpoint last_opt {A : Type} (l : list A) : option A :=
  match l with
  | [] => None
  | [x] => Some x
  | _ :: t => last_opt t
  end.

Lemma last_opt_app_cons :
  forall (A : Type) (l : list A) (x : A) (t : list A),
    last_opt (l ++ x :: t) = last_opt (x :: t).
Proof.
  induction l as [|a l IH]; intros x t.
  - reflexivity.
  - simpl. destruct (l ++ x :: t) eqn:E.
    + exfalso. destruct l; discriminate.
    + rewrite <- E. apply IH.
Qed.

Lemma last_opt_app :
  forall (A : Type) (l : list A) (x : A), last_opt (l ++ [x]) = Some x.
Proof.
  intros A l x. exact (last_opt_app_cons A l x []).
Qed.

(* ===================================================================== *)
(* Part 1. The blob store: per-path append-only version list.            *)
(*                                                                       *)
(* A path's history is a list of versions; list position i is server     *)
(* version i+1 (ingest.go:90-93 assigns nextVersion = latest+1, i.e.     *)
(* ARRIVAL order).  A version is either Content sha or a Tombstone       *)
(* (ingest.go:63: SHA == "" means delete; ingest.go:102-105 leaves       *)
(* ContentSHA nil => NULL content_sha in file_versions).                 *)
(* ===================================================================== *)

Definition sha := nat.

Inductive entry : Type :=
| Content : sha -> entry
| Tomb : entry.

Definition entry_eqb (a b : entry) : bool :=
  match a, b with
  | Content x, Content y => Nat.eqb x y
  | Tomb, Tomb => true
  | _, _ => false
  end.

Lemma entry_eqb_refl : forall e, entry_eqb e e = true.
Proof.
  destruct e; simpl; [apply Nat.eqb_refl | reflexivity].
Qed.

(* Snapshot(path): the latest version if it is Content, else the path is *)
(* absent.  Models store.go:158-182 CurrentSnapshot: MAX(version) per    *)
(* path, WHERE content_sha IS NOT NULL.                                  *)
Definition snapshot (vs : list entry) : option sha :=
  match last_opt vs with
  | Some (Content s) => Some s
  | _ => None
  end.

Lemma snapshot_app :
  forall vs m,
    snapshot (vs ++ [m]) =
    match m with Content s => Some s | Tomb => None end.
Proof.
  intros vs m. unfold snapshot. rewrite last_opt_app. reflexivity.
Qed.

(* One manifest entry hitting the server (ingest.go:80-110):             *)
(*  - if it equals the latest version's content it is SKIPPED            *)
(*    (ingest.go:85-88 with sameContent, ingest.go:130-135 -- note that   *)
(*    sameContent compares ONLY the sha, never mode/mtime);              *)
(*  - otherwise it is APPENDED as version latest+1.                      *)
Definition ingest1 (vs : list entry) (m : entry) : list entry :=
  match last_opt vs with
  | Some l => if entry_eqb l m then vs else vs ++ [m]
  | None => vs ++ [m]
  end.

(* --------------------------------------------------------------------- *)
(* Theorem 1 -- Append-only.                                              *)
(* Ingesting never removes or mutates an existing version: the new state *)
(* is always the old state plus a (possibly empty) suffix.  This is the  *)
(* formal shadow of store.go:87-97 AppendVersion being a bare INSERT     *)
(* (store.go contains no UPDATE or DELETE on file_versions at all).      *)
(* --------------------------------------------------------------------- *)

Theorem ingest_append_only :
  forall vs m, exists suffix, ingest1 vs m = vs ++ suffix.
Proof.
  intros vs m. unfold ingest1.
  destruct (last_opt vs) as [l|].
  - destruct (entry_eqb l m).
    + exists []. rewrite app_nil_r. reflexivity.
    + exists [m]. reflexivity.
  - exists [m]. reflexivity.
Qed.

(* Anything readable before an ingest is readable, unchanged, after it.  *)
Theorem ingest_preserves_reads :
  forall vs m i e,
    nth_error vs i = Some e -> nth_error (ingest1 vs m) i = Some e.
Proof.
  intros vs m i e H.
  destruct (ingest_append_only vs m) as [suffix Hs].
  rewrite Hs.
  assert (Hlen : i < length vs).
  { apply nth_error_Some. rewrite H. discriminate. }
  rewrite nth_error_app1 by assumption.
  exact H.
Qed.

(* A whole manifest (sequence of entries, processed in order by the      *)
(* scanner loop ingest.go:48-111) still only ever appends.               *)
Definition ingest_seq (vs : list entry) (ms : list entry) : list entry :=
  fold_left ingest1 ms vs.

Theorem ingest_seq_append_only :
  forall ms vs, exists suffix, ingest_seq vs ms = vs ++ suffix.
Proof.
  induction ms as [|m ms IH]; intros vs.
  - exists []. rewrite app_nil_r. reflexivity.
  - destruct (ingest_append_only vs m) as [s1 E1].
    destruct (IH (ingest1 vs m)) as [s2 E2].
    exists (s1 ++ s2).
    unfold ingest_seq in *. simpl.
    rewrite E2, E1. rewrite <- app_assoc. reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* Ingest dedup: skipping a duplicate preserves snapshot equality.       *)
(* ingest.go:85-88: an entry equal to the latest version is skipped;     *)
(* had it been appended instead, the snapshot would be the same.         *)
(* --------------------------------------------------------------------- *)

Theorem dedup_skip_sound :
  forall vs m,
    last_opt vs = Some m ->
    ingest1 vs m = vs /\ snapshot (vs ++ [m]) = snapshot vs.
Proof.
  intros vs m H. split.
  - unfold ingest1. rewrite H, entry_eqb_refl. reflexivity.
  - rewrite snapshot_app. unfold snapshot. rewrite H.
    destruct m; reflexivity.
Qed.

(* Pushing content always makes it the snapshot -- whether it commits a   *)
(* new version or is skipped as a duplicate of the current latest.       *)
Theorem push_content_visible :
  forall vs s, snapshot (ingest1 vs (Content s)) = Some s.
Proof.
  intros vs s. unfold ingest1.
  destruct (last_opt vs) as [l|] eqn:Hl.
  - destruct (entry_eqb l (Content s)) eqn:He.
    + destruct l as [s'|]; simpl in He; [|discriminate].
      apply Nat.eqb_eq in He. subst s'.
      unfold snapshot. rewrite Hl. reflexivity.
    + rewrite snapshot_app. reflexivity.
  - rewrite snapshot_app. reflexivity.
Qed.

(* Pushing a delete always hides the path.                               *)
Theorem push_delete_hides :
  forall vs, snapshot (ingest1 vs Tomb) = None.
Proof.
  intros vs. unfold ingest1.
  destruct (last_opt vs) as [l|] eqn:Hl.
  - destruct (entry_eqb l Tomb) eqn:He.
    + destruct l; simpl in He; [discriminate|].
      unfold snapshot. rewrite Hl. reflexivity.
    + rewrite snapshot_app. reflexivity.
  - rewrite snapshot_app. reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* Two-phase ingest (ingest.go:64-77): a Content entry whose blob is     *)
(* missing from the blob store is NOT committed -- it lands in the "need" *)
(* list and the version list is untouched.  After the client uploads the *)
(* blob (push.go:181-203: putBlob then re-send the same manifest), the   *)
(* re-sent entry commits exactly as a normal ingest.                     *)
(* --------------------------------------------------------------------- *)

Definition blob_store := list sha.

Definition entry_available (bs : blob_store) (m : entry) : bool :=
  match m with
  | Content s => existsb (Nat.eqb s) bs
  | Tomb => true                      (* deletes need no blob: ingest.go:63-64 *)
  end.

Definition ingest_gated (bs : blob_store) (vs : list entry) (m : entry)
  : list entry :=
  if entry_available bs m then ingest1 vs m else vs.

Theorem missing_blob_not_committed :
  forall bs vs m, entry_available bs m = false -> ingest_gated bs vs m = vs.
Proof.
  intros bs vs m H. unfold ingest_gated. rewrite H. reflexivity.
Qed.

Theorem resend_after_upload_commits :
  forall bs vs s,
    ingest_gated (s :: bs) vs (Content s) = ingest1 vs (Content s).
Proof.
  intros bs vs s. unfold ingest_gated. simpl.
  rewrite Nat.eqb_refl. reflexivity.
Qed.

(* Gating never un-appends either: the two-phase protocol is append-only *)
(* end to end.                                                           *)
Theorem gated_append_only :
  forall bs vs m, exists suffix, ingest_gated bs vs m = vs ++ suffix.
Proof.
  intros bs vs m. unfold ingest_gated.
  destruct (entry_available bs m).
  - apply ingest_append_only.
  - exists []. rewrite app_nil_r. reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* Theorem 4 -- Tombstone no-resurrection.                                *)
(* Once some version is a tombstone, the snapshot excludes the path      *)
(* unless a LATER Content version exists; and a later push re-creating   *)
(* the file wins.                                                        *)
(* --------------------------------------------------------------------- *)

Lemma all_tomb_last :
  forall (post : list entry),
    (forall s, ~ In (Content s) post) ->
    last_opt (Tomb :: post) = Some Tomb.
Proof.
  induction post as [|x t IH]; intros H.
  - reflexivity.
  - destruct x as [s|].
    + exfalso. apply (H s). left. reflexivity.
    + change (last_opt (Tomb :: Tomb :: t)) with (last_opt (Tomb :: t)).
      apply IH. intros s Hin. apply (H s). right. exact Hin.
Qed.

(* If version |pre|+1 is a tombstone and no later version is Content,    *)
(* the path is absent from the snapshot.                                 *)
Theorem no_resurrection :
  forall pre post,
    (forall s, ~ In (Content s) post) ->
    snapshot (pre ++ Tomb :: post) = None.
Proof.
  intros pre post H. unfold snapshot.
  rewrite last_opt_app_cons.
  rewrite (all_tomb_last post H). reflexivity.
Qed.

(* A later Content version wins over any tombstone history: re-creating  *)
(* a deleted file works.                                                 *)
Theorem recreate_wins :
  forall vs s, snapshot (vs ++ [Content s]) = Some s.
Proof.
  intros vs s. rewrite snapshot_app. reflexivity.
Qed.

Theorem push_after_tombstone_wins :
  forall vs s,
    last_opt vs = Some Tomb ->
    snapshot (ingest1 vs (Content s)) = Some s.
Proof.
  intros vs s H. apply push_content_visible.
Qed.

(* ===================================================================== *)
(* Part 2. Honesty lemma A -- the server has NO mtime-LWW.                *)
(*                                                                       *)
(* Versions carry the client-reported mtime (ingest.go:94-101 stores     *)
(* e.MtimeNS) but the server never READS it to order versions: arrival   *)
(* order alone decides (ingest.go:90-93).  Two hosts pushing the same    *)
(* path therefore race, and an entry with an OLDER mtime can become the  *)
(* snapshot simply by arriving later.  We exhibit the interleaving.      *)
(* ===================================================================== *)

Record fversion : Type := mkFV { fv_entry : entry; fv_mtime : nat }.

Definition msnapshot (vs : list fversion) : option (sha * nat) :=
  match last_opt vs with
  | Some f => match fv_entry f with
              | Content s => Some (s, fv_mtime f)
              | Tomb => None
              end
  | None => None
  end.

Lemma msnapshot_app :
  forall vs f,
    msnapshot (vs ++ [f]) =
    match fv_entry f with
    | Content s => Some (s, fv_mtime f)
    | Tomb => None
    end.
Proof.
  intros vs f. unfold msnapshot. rewrite last_opt_app. reflexivity.
Qed.

Definition ingest_m (vs : list fversion) (f : fversion) : list fversion :=
  match last_opt vs with
  | Some l => if entry_eqb (fv_entry l) (fv_entry f) then vs else vs ++ [f]
  | None => vs ++ [f]
  end.

(* The general fact: whatever mtimes the history carries, the last       *)
(* arrival that actually commits determines the snapshot.  mtime is      *)
(* stored but never consulted.                                           *)
Theorem server_ignores_mtime :
  forall vs f s,
    fv_entry f = Content s ->
    (forall l, last_opt vs = Some l ->
               entry_eqb (fv_entry l) (fv_entry f) = false) ->
    msnapshot (ingest_m vs f) = Some (s, fv_mtime f).
Proof.
  intros vs f s Hf Hne. unfold ingest_m.
  destruct (last_opt vs) as [l|] eqn:Hl.
  - rewrite (Hne l eq_refl).
    rewrite msnapshot_app, Hf. reflexivity.
  - rewrite msnapshot_app, Hf. reflexivity.
Qed.

(* The concrete counterexample: host A pushes sha 1 with mtime 10 (the   *)
(* NEWER file); host B pushes sha 2 with mtime 5 (the OLDER file) but    *)
(* arrives second.  The server keeps sha 2: the older mtime overwrote    *)
(* the newer one.  This interleaving exists; nothing in the server       *)
(* prevents it.                                                          *)
Theorem no_mtime_lww_counterexample :
  let pushA := mkFV (Content 1) 10 in
  let pushB := mkFV (Content 2) 5 in
  msnapshot (ingest_m (ingest_m [] pushA) pushB) = Some (2, 5)
  /\ fv_mtime pushB < fv_mtime pushA.
Proof.
  split.
  - reflexivity.
  - simpl. lia.
Qed.

(* ===================================================================== *)
(* Part 3. The log store: a G-set of (path, line_hash) with immutable    *)
(* first_seen.                                                           *)
(*                                                                       *)
(* log_lines has PRIMARY KEY (path, line_hash) (migrations.sql) and      *)
(* AppendLogLine is INSERT OR IGNORE (store.go:123-136): a duplicate     *)
(* key leaves the stored row -- including its first_seen -- untouched.     *)
(* A key is (path, line_hash) as a pair of nats; since line_hash is      *)
(* sha256(contents) (log.go:57-60), equal keys imply equal contents,     *)
(* so key-level reasoning is content-level reasoning.                    *)
(* ===================================================================== *)

Definition key : Type := (nat * nat)%type.

Definition key_eqb (a b : key) : bool :=
  Nat.eqb (fst a) (fst b) && Nat.eqb (snd a) (snd b).

Lemma key_eqb_eq : forall a b, key_eqb a b = true <-> a = b.
Proof.
  intros [pa ha] [pb hb]. unfold key_eqb. simpl.
  rewrite andb_true_iff, !Nat.eqb_eq.
  split.
  - intros [E1 E2]. subst. reflexivity.
  - intros E. inversion E. auto.
Qed.

Lemma key_eqb_refl : forall a, key_eqb a a = true.
Proof.
  intros a. apply key_eqb_eq. reflexivity.
Qed.

Record lline : Type := mkLine { lkey : key; lseen : nat }.

Definition lstore := list lline.

Definition has_key (st : lstore) (k : key) : bool :=
  existsb (fun l => key_eqb (lkey l) k) st.

(* AppendLogLine: INSERT OR IGNORE on the (path, line_hash) key.         *)
Definition insertL (st : lstore) (l : lline) : lstore :=
  if has_key st (lkey l) then st else l :: st.

(* --------------------------------------------------------------------- *)
(* Theorem 3 -- Log dedup soundness (CRDT G-set argument).                *)
(* --------------------------------------------------------------------- *)

(* Inserting a duplicate leaves the store LITERALLY unchanged -- in       *)
(* particular every stored first_seen is immutable, exactly the          *)
(* INSERT OR IGNORE semantics of store.go:123-136.                       *)
Theorem log_dedup_sound :
  forall st l, has_key st (lkey l) = true -> insertL st l = st.
Proof.
  intros st l H. unfold insertL. rewrite H. reflexivity.
Qed.

Lemma has_key_insert :
  forall st l k,
    has_key (insertL st l) k = has_key st k || key_eqb (lkey l) k.
Proof.
  intros st l k. unfold insertL.
  destruct (has_key st (lkey l)) eqn:Hd.
  - destruct (key_eqb (lkey l) k) eqn:Hk.
    + apply key_eqb_eq in Hk. subst k. rewrite Hd. reflexivity.
    + rewrite orb_false_r. reflexivity.
  - unfold has_key. simpl. apply orb_comm.
Qed.

(* Merging two stores = inserting every line of one into the other;     *)
(* this is what a push replays against the server, and what a pull      *)
(* replays against the local file.                                       *)
Fixpoint merge (a b : lstore) : lstore :=
  match b with
  | [] => a
  | x :: b' => merge (insertL a x) b'
  end.

Lemma merge_has_key :
  forall b a k, has_key (merge a b) k = has_key a k || has_key b k.
Proof.
  induction b as [|x b IH]; intros a k.
  - simpl. rewrite orb_false_r. reflexivity.
  - change (merge a (x :: b)) with (merge (insertL a x) b).
    change (has_key (x :: b) k) with (key_eqb (lkey x) k || has_key b k).
    rewrite IH, has_key_insert, orb_assoc. reflexivity.
Qed.

(* Observational (key-set) equality.                                     *)
Definition st_eq (a b : lstore) : Prop :=
  forall k, has_key a k = has_key b k.

Theorem merge_idem : forall a, st_eq (merge a a) a.
Proof.
  intros a k. rewrite merge_has_key. apply orb_diag.
Qed.

Theorem merge_comm : forall a b, st_eq (merge a b) (merge b a).
Proof.
  intros a b k. rewrite !merge_has_key. apply orb_comm.
Qed.

Theorem merge_assoc :
  forall a b c, st_eq (merge (merge a b) c) (merge a (merge b c)).
Proof.
  intros a b c k. rewrite !merge_has_key.
  symmetry. apply orb_assoc.
Qed.

(* ===================================================================== *)
(* Part 4. Theorem 2 -- Log pull no-loss and cursor safety.               *)
(*                                                                       *)
(* pull(c) is exactly { l | first_seen l > c }: store.go:138-156         *)
(* LogLinesSince uses WHERE first_seen > ?, served by log.go:115-150.    *)
(* The client cursor lives in state.json (state.go:14 LogCursor) and is  *)
(* advanced to max first_seen of a COMPLETED pull only (pull.go:65-74:   *)
(* the cursor is written after every mergeLogFile succeeded; any error   *)
(* aborts before SaveState).                                             *)
(* ===================================================================== *)

Definition pull (c : nat) (st : lstore) : list lline :=
  filter (fun l => c <? lseen l) st.

(* Everything newer than the cursor is in the pull -- nothing is lost.    *)
Theorem pull_no_loss :
  forall st c l, In l st -> c < lseen l -> In l (pull c st).
Proof.
  intros st c l Hin Hlt. apply filter_In. split.
  - exact Hin.
  - apply Nat.ltb_lt. exact Hlt.
Qed.

(* (local lines) ∪ pull(c) covers every line with first_seen > c.        *)
Theorem union_covers :
  forall (local : list lline) st c l,
    In l st -> c < lseen l -> In l (local ++ pull c st).
Proof.
  intros local st c l Hin Hlt.
  apply in_or_app. right. apply pull_no_loss; assumption.
Qed.

(* The client invariant: every server line at or below the cursor is     *)
(* already local.  This is what LogCursor MEANS.                         *)
Definition covered (c : nat) (local : list lline) (st : lstore) : Prop :=
  forall l, In l st -> lseen l <= c -> In l local.

(* A completed pull converges: under the invariant, local ∪ pull(c) is   *)
(* the FULL server set.                                                  *)
Theorem completed_pull_converges :
  forall c local st,
    covered c local st ->
    forall l, In l st -> In l (local ++ pull c st).
Proof.
  intros c local st Hcov l Hin.
  destruct (le_lt_dec (lseen l) c) as [Hle|Hgt].
  - apply in_or_app. left. apply (Hcov l Hin Hle).
  - apply in_or_app. right. apply pull_no_loss; assumption.
Qed.

(* New cursor after a completed pull = max first_seen over the pulled    *)
(* lines, seeded with the old cursor (pull.go:199, 212-214: maxFirstSeen *)
(* starts at since and only grows).                                      *)
Fixpoint max_seen (ls : list lline) (c : nat) : nat :=
  match ls with
  | [] => c
  | l :: t => max_seen t (Nat.max c (lseen l))
  end.

Lemma max_seen_ge : forall ls c, c <= max_seen ls c.
Proof.
  induction ls as [|l t IH]; intros c; simpl.
  - lia.
  - specialize (IH (Nat.max c (lseen l))). lia.
Qed.

(* The cursor never moves backwards (pull.go:69-71 additionally guards   *)
(* with newCursor > state.LogCursor).                                    *)
Theorem cursor_monotone : forall c st, c <= max_seen (pull c st) c.
Proof.
  intros c st. apply max_seen_ge.
Qed.

(* Advancing the cursor after a COMPLETED pull re-establishes the        *)
(* invariant, for the new cursor -- so repeated pulls stay lossless.      *)
Theorem cursor_advance_safe :
  forall c local st,
    covered c local st ->
    covered (max_seen (pull c st) c) (local ++ pull c st) st.
Proof.
  intros c local st Hcov l Hin _.
  apply completed_pull_converges; assumption.
Qed.

(* Cursor safety on interruption: if a pull dies half-way the cursor is  *)
(* NOT advanced (pull.go: any mergeLogFile error returns before the      *)
(* cursor is stored), so whatever partial data was written can only      *)
(* grow `local`, and the invariant survives.  The next pull re-fetches.  *)
(* CAVEAT: a truncated /log/since stream is NOT detected as an           *)
(* interruption -- the client sees a well-formed prefix and advances the  *)
(* cursor anyway.  See truncated_stream_tie_skipped (Part 6, C4).        *)
Theorem interrupted_pull_safe :
  forall c local st received,
    covered c local st ->
    covered c (local ++ received) st.
Proof.
  intros c local st received Hcov l Hin Hle.
  apply in_or_app. left. apply (Hcov l Hin Hle).
Qed.

(* Contrapositive honesty: advancing the cursor WITHOUT completing the   *)
(* pull would break the invariant.  Cursor-on-completion is load-bearing.*)
Theorem cursor_skip_unsafe :
  exists c c' (local : list lline) (st : lstore),
    covered c local st /\ c < c' /\ ~ covered c' local st.
Proof.
  exists 0, 1, [], [mkLine (0, 0) 1].
  split; [| split].
  - intros l Hin Hle.
    destruct Hin as [E|F]; [subst l; simpl in Hle; lia | destruct F].
  - lia.
  - intros Hc.
    apply (Hc (mkLine (0, 0) 1)).
    + left. reflexivity.
    + simpl. lia.
Qed.

(* New lines keep the invariant ONLY IF their first_seen exceeds the     *)
(* client's cursor.  That is an ASSUMPTION, not something the code       *)
(* enforces: log.go:57-63 evaluates time.Now().UnixNano() in the         *)
(* handler loop, NON-atomically with the INSERT at log.go:66             *)
(* (SetMaxOpenConns(1), store.go:37, serializes statements -- not         *)
(* stamp+insert pairs), so a line can commit with a first_seen at or     *)
(* below an already-issued cursor even on a perfectly monotone clock.    *)
(* See monotone_stamp_race_unsafe (Part 6, C3).                          *)
Theorem append_preserves_covered :
  forall c local st l,
    covered c local st -> c < lseen l -> covered c local (insertL st l).
Proof.
  intros c local st l Hcov Hgt l' Hin Hle.
  unfold insertL in Hin.
  destruct (has_key st (lkey l)).
  - apply (Hcov l' Hin Hle).
  - destruct Hin as [E|Hin].
    + subst l'. exfalso. lia.
    + apply (Hcov l' Hin Hle).
Qed.

(* Honesty: WITHOUT that assumption the invariant is not preserved -- a   *)
(* line inserted with first_seen <= cursor is invisible to every future  *)
(* incremental pull.  Reachable two ways: a server clock regression, OR  *)
(* -- with no clock anomaly at all -- the stamp/insert race exhibited by   *)
(* monotone_stamp_race_unsafe (Part 6, C3).                              *)
Theorem clock_regression_unsafe :
  exists c (local : list lline) (st : lstore) l,
    covered c local st /\ lseen l <= c /\ ~ covered c local (insertL st l).
Proof.
  exists 1, [], [], (mkLine (0, 0) 1).
  split; [| split].
  - intros l' F. destruct F.
  - simpl. lia.
  - intros Hc.
    apply (Hc (mkLine (0, 0) 1)).
    + simpl. left. reflexivity.
    + simpl. lia.
Qed.

(* ===================================================================== *)
(* Part 5. Honesty lemma B -- pull --full rewrites log files.             *)
(*                                                                       *)
(* Normal pull merges: mergeLogFile (pull.go:250-277) reads the local    *)
(* file, dedups by content hash, and APPENDS only -- local lines are      *)
(* never touched.  With --full (pull.go:246-248 + rewriteLogFile,        *)
(* pull.go:280-306) the local file is REPLACED by exactly the            *)
(* server-derived lines.  That is loss iff a local line is missing from  *)
(* the server.  Part 6 shows the code does NOT establish pushed-implies- *)
(* on-the-server (C1: redaction; push.go additionally warn-and-skips     *)
(* failing files and holds back unterminated trailing lines), and that   *)
(* --full corrupts the NEXT push even when the rewrite itself loses      *)
(* nothing (C2: stale LogOffsets).                                       *)
(* ===================================================================== *)

(* Non-full merge (the default): local ++ (incoming minus duplicates).   *)
(* NOTE: this models a COMPLETE dedup set.  mergeLogFile only dedups     *)
(* against the lines its scanner managed to read before an unreported    *)
(* error (pull.go:251-260 ignores sc.Err()) -- see                        *)
(* partial_dedup_duplicates (Part 6, C5).                                *)
Definition merge_nonfull (local incoming : list lline) : list lline :=
  local ++ filter (fun l => negb (has_key local (lkey l))) incoming.

Theorem nonfull_merge_preserves_local :
  forall local incoming l, In l local -> In l (merge_nonfull local incoming).
Proof.
  intros local incoming l H.
  apply in_or_app. left. exact H.
Qed.

Theorem nonfull_merge_covers_incoming :
  forall local incoming l,
    In l incoming -> has_key (merge_nonfull local incoming) (lkey l) = true.
Proof.
  intros local incoming l Hin.
  destruct (has_key local (lkey l)) eqn:Hl.
  - unfold merge_nonfull, has_key. rewrite existsb_app.
    unfold has_key in Hl. rewrite Hl. reflexivity.
  - unfold merge_nonfull, has_key. rewrite existsb_app.
    apply orb_true_iff. right.
    unfold has_key in Hl.
    apply existsb_exists. exists l. split.
    + apply filter_In. split; [exact Hin | cbv beta; rewrite Hl; reflexivity].
    + apply key_eqb_refl.
Qed.

(* --full: the local file becomes exactly the server's lines.            *)
Definition full_rewrite (local server_lines : list lline) : list lline :=
  server_lines.

(* Set-preservation of the rewrite ITSELF, under the precondition that   *)
(* every local line is on the server.  "Safe" claims nothing more: a     *)
(* completed `cccp push` does NOT establish the precondition (Part 6,    *)
(* C1), and even when it holds, --full corrupts the next push through    *)
(* the stale byte offset (Part 6, C2).                                   *)
Theorem full_pull_safe_if_pushed :
  forall local sv,
    (forall l, In l local -> In l sv) ->
    forall l, In l local -> In l (full_rewrite local sv).
Proof.
  intros local sv Hsub l Hl. apply Hsub. exact Hl.
Qed.

(* Precondition violated => provable loss: an unpushed local line is     *)
(* destroyed by the rewrite.                                             *)
Theorem full_pull_lossy_if_unpushed :
  exists (local sv : list lline) l,
    In l local /\ ~ In l sv /\ ~ In l (full_rewrite local sv).
Proof.
  exists [mkLine (0, 0) 0], [], (mkLine (0, 0) 0).
  split.
  - left. reflexivity.
  - split; intros F; destruct F.
Qed.

(* ===================================================================== *)
(* Part 6. Honesty lemmas C -- the log pipeline does NOT establish the    *)
(* preconditions of Parts 4-5.                                           *)
(*                                                                       *)
(* Each lemma below is a concrete failing interleaving of the actual     *)
(* code paths, cited inline.  The positive theorems above remain true    *)
(* AS CONDITIONALS; these lemmas prove the code does not discharge the   *)
(* conditions.                                                           *)
(* ===================================================================== *)

(* --------------------------------------------------------------------- *)
(* C1.  Redaction breaks the claim that a completed push puts every      *)
(* local line on the server.  push.go:355-358 sends the REDACTED line    *)
(* (toSend = cheka.RedactLine(line)): the                                *)
(* server stores the redacted twin, keyed by ITS content hash            *)
(* (log.go:57-60), never the local original -- and `cccp watch` defaults  *)
(* --cheka-redact to true.  So even after a fully successful push a      *)
(* redacted local line is provably NOT on the server, and --full         *)
(* (full_rewrite) destroys the original.  Redaction is modeled as an     *)
(* arbitrary function r on line contents; keys carry the content hash,   *)
(* so the server-side twin has key (path, r hash).                       *)
(* --------------------------------------------------------------------- *)

Definition redact_line (r : nat -> nat) (l : lline) : lline :=
  mkLine (fst (lkey l), r (snd (lkey l))) (lseen l).

Theorem redacted_push_not_subset :
  exists (r : nat -> nat) (local sv : list lline) l,
    sv = map (redact_line r) local /\  (* a COMPLETED, successful push *)
    In l local /\
    ~ In l sv /\                       (* the original is not on the server *)
    ~ In l (full_rewrite local sv).    (* so --full destroys it *)
Proof.
  exists S, [mkLine (0, 0) 0], [mkLine (0, 1) 0], (mkLine (0, 0) 0).
  split; [reflexivity|].
  split; [left; reflexivity|].
  split; intros [E|[]]; discriminate.
Qed.

(* --------------------------------------------------------------------- *)
(* C2.  --full leaves LogOffsets stale: the NEXT push commits a garbage  *)
(* fragment as a permanent server line.  Pull never touches              *)
(* state.LogOffsets (pull.go:34-77 mutates only KnownPulledBlobPaths and *)
(* LogCursor), while rewriteLogFile (pull.go:280-306) re-materializes    *)
(* the file in SERVER sort order (suggested_time, first_seen)            *)
(* (pull.go:221-226).  The preserved byte offset -- a line boundary of    *)
(* the OLD file -- can land mid-line in the NEW file; the next push       *)
(* seeks there (push.go:330-334) and uploads the suffix, which the       *)
(* server dedups under the fragment's OWN hash and commits forever.      *)
(*                                                                       *)
(* Bytes are nats, newline = 10; a file is the newline-joined bytes of   *)
(* its lines; split_nl is push's splitter with the trailing partial      *)
(* line held back (push.go:339-350).                                     *)
(* --------------------------------------------------------------------- *)

Definition bline := list nat.

Fixpoint flat_file (ls : list bline) : list nat :=
  match ls with
  | [] => []
  | l :: t => l ++ 10 :: flat_file t
  end.

Fixpoint split_nl (bs : list nat) (cur : list nat) : list bline :=
  match bs with
  | [] => []                       (* partial trailing line: held back *)
  | b :: t => if Nat.eqb b 10
              then rev cur :: split_nl t []
              else split_nl t (b :: cur)
  end.

Definition push_payload (file : list nat) (offset : nat) : list bline :=
  split_nl (skipn offset file) [].

Theorem stale_offset_fragment :
  exists (old_lines new_lines : list bline) (offset : nat) (frag : bline),
    offset = length (flat_file old_lines) /\     (* fully-pushed offset *)
    push_payload (flat_file old_lines) offset = [] /\
    (* after --full re-sorts another host's line in front:              *)
    push_payload (flat_file new_lines) offset = [frag] /\
    ~ In frag new_lines /\           (* not a line of the new file      *)
    ~ In frag old_lines.             (* nor of the old one: pure garbage *)
Proof.
  exists [[1; 2; 3]], [[7; 7]; [1; 2; 3]], 4, [2; 3].
  split; [reflexivity|].
  split; [reflexivity|].
  split; [reflexivity|].
  split.
  - intros [E|[E|[]]]; discriminate.
  - intros [E|[]]; discriminate.
Qed.

(* --------------------------------------------------------------------- *)
(* C3.  The cursor can permanently skip lines on a PERFECTLY MONOTONE    *)
(* clock.  append_preserves_covered needs first_seen > cursor for new    *)
(* lines; the code does not guarantee it.  FirstSeen is evaluated in     *)
(* the handler loop (log.go:57-63) NON-atomically with the INSERT        *)
(* (log.go:66); SetMaxOpenConns(1) (store.go:37) serializes statements,  *)
(* not stamp+insert pairs.  Interleaving: handler A stamps t=1; handler  *)
(* B stamps t=2 and commits; a pull completes and issues cursor 2; A's   *)
(* INSERT then commits its t=1 line.  The line sits at/below the cursor  *)
(* forever -- clock_regression_unsafe's state, reached with strictly      *)
(* increasing timestamps.  (The README previously blamed only NTP steps  *)
(* backwards; that attribution was wrong.)                               *)
(* --------------------------------------------------------------------- *)

Inductive op : Type :=
| OStamp  (id t : nat)  (* handler id evaluates time.Now()  (log.go:63) *)
| OInsert (id : nat)    (* handler id's INSERT commits      (log.go:66) *)
| OPull.                (* a client COMPLETES a pull and advances its   *)
                        (* cursor exactly as cursor_advance_safe says   *)

Record wstate := mkW {
  wpend   : list (nat * nat);
  wstore  : lstore;
  wlocal  : list lline;
  wcursor : nat
}.

Fixpoint lookup_pend (p : list (nat * nat)) (id : nat) : option nat :=
  match p with
  | [] => None
  | (i, t) :: r => if Nat.eqb i id then Some t else lookup_pend r id
  end.

Definition wstep (s : wstate) (o : op) : wstate :=
  match o with
  | OStamp id t => mkW ((id, t) :: wpend s) (wstore s) (wlocal s) (wcursor s)
  | OInsert id =>
      match lookup_pend (wpend s) id with
      | Some t =>
          mkW (wpend s) (insertL (wstore s) (mkLine (id, id) t))
              (wlocal s) (wcursor s)
      | None => s
      end
  | OPull =>
      mkW (wpend s) (wstore s)
          (wlocal s ++ pull (wcursor s) (wstore s))
          (max_seen (pull (wcursor s) (wstore s)) (wcursor s))
  end.

Definition wrun (t : list op) : wstate :=
  fold_left wstep t (mkW [] [] [] 0).

(* Stamps appear in the trace in strictly increasing order (1 then 2):   *)
(* the server clock never steps back.                                    *)
Definition race_trace : list op :=
  [OStamp 1 1; OStamp 2 2; OInsert 2; OPull; OInsert 1].

Lemma race_final :
  wrun race_trace =
  mkW [(2, 2); (1, 1)]
      [mkLine (1, 1) 1; mkLine (2, 2) 2]
      [mkLine (2, 2) 2]
      2.
Proof. reflexivity. Qed.

Theorem monotone_stamp_race_unsafe :
  In (mkLine (1, 1) 1) (wstore (wrun race_trace))          (* committed *)
  /\ lseen (mkLine (1, 1) 1) <= wcursor (wrun race_trace)  (* below cursor *)
  /\ ~ In (mkLine (1, 1) 1) (wlocal (wrun race_trace))     (* never pulled *)
  /\ ~ covered (wcursor (wrun race_trace)) (wlocal (wrun race_trace))
               (wstore (wrun race_trace)).
Proof.
  rewrite race_final.
  split; [left; reflexivity|].
  split; [simpl; lia|].
  split.
  - intros [E|[]]. discriminate.
  - intros Hc.
    destruct (Hc (mkLine (1, 1) 1) (or_introl eq_refl) (le_S 1 1 (le_n 1)))
      as [E|[]].
    discriminate.
Qed.

(* --------------------------------------------------------------------- *)
(* C4.  A truncated /log/since stream looks like a COMPLETED pull.       *)
(* handleLogSince streams after headers are flushed; a store error       *)
(* mid-stream just returns (log.go:142-145), so the client receives a    *)
(* well-formed ndjson PREFIX, and pullLogs treats it as complete and     *)
(* advances the cursor to the max first_seen it saw (pull.go:199-234,    *)
(* then 69-71).  The server orders by first_seen ASC (store.go:138-156), *)
(* so strictly-greater lines are re-fetched later -- but a line TYING     *)
(* the last received first_seen at the truncation point is <= cursor     *)
(* forever.  interrupted_pull_safe does not apply: its premise (cursor   *)
(* untouched) is exactly what fails.  This instantiates                  *)
(* cursor_skip_unsafe from inside the server code, with no clock         *)
(* anomaly and no client bug.                                            *)
(* --------------------------------------------------------------------- *)

Theorem truncated_stream_tie_skipped :
  exists (sent : list lline) (missing : lline) (st : lstore) (c' : nat),
    st = sent ++ [missing] /\          (* server stream order; tie last  *)
    c' = max_seen sent 0 /\            (* cursor from the truncated body *)
    lseen missing <= c' /\             (* the tie                        *)
    ~ In missing sent /\
    ~ In missing (pull c' st) /\       (* never re-fetched               *)
    ~ covered c' sent st.
Proof.
  exists [mkLine (0, 0) 5], (mkLine (0, 1) 5),
         [mkLine (0, 0) 5; mkLine (0, 1) 5], 5.
  split; [reflexivity|].
  split; [reflexivity|].
  split; [simpl; lia|].
  split; [intros [E|[]]; discriminate|].
  split; [simpl; intros [] |].
  intros Hc.
  destruct (Hc (mkLine (0, 1) 5) (or_intror (or_introl eq_refl)) (le_n 5))
    as [E|[]].
  discriminate.
Qed.

(* --------------------------------------------------------------------- *)
(* C5.  The client-side merge is NOT insertL: mergeLogFile ignores       *)
(* sc.Err() (pull.go:251-260).  A local line longer than MaxScanLine     *)
(* (4 MB, walk.go:16) or any read error silently truncates the           *)
(* `existing` dedup set, after which pulled duplicates are APPENDED.     *)
(* merge_nonfull models a COMPLETE dedup set; the code only dedups       *)
(* against the VISIBLE subset -- and then file-level idempotence fails:   *)
(* pulling your own pushed line duplicates it on disk.                   *)
(* --------------------------------------------------------------------- *)

Definition merge_nonfull_partial (local visible incoming : list lline)
  : list lline :=
  local ++ filter (fun l => negb (has_key visible (lkey l))) incoming.

Theorem partial_dedup_duplicates :
  exists (local visible incoming : list lline) l,
    (forall x, In x visible -> In x local) /\ (* truncated scan: visible⊆local *)
    In l local /\
    In l incoming /\
    merge_nonfull_partial local visible incoming = local ++ [l].
Proof.
  exists [mkLine (0, 0) 0], [], [mkLine (0, 0) 0], (mkLine (0, 0) 0).
  split; [intros x [] |].
  split; [left; reflexivity|].
  split; [left; reflexivity|].
  reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* C6.  A single oversize line permanently wedges log sync for its file. *)
(* The client has NO line-length limit on push (io.ReadAll+bytes.Split,  *)
(* push.go:335-350) but the server's request scanner caps lines at       *)
(* maxScanLine = 4 MB (api/server.go:20, log.go:41-42).  On an oversize  *)
(* line the scanner stops and the request 400s (log.go:78-81) -- but      *)
(* earlier lines of the SAME request were already committed (per-line    *)
(* INSERTs, log.go:43-77, no transaction).  The client warns and keeps   *)
(* the old offset (push.go:218-222), so every retry replays the          *)
(* identical payload: the offset never advances and every line AFTER     *)
(* the oversize one never reaches the server.  This is a LIVENESS        *)
(* failure -- the total folds of Parts 3-4 cannot express it, which is    *)
(* why it needs its own lemma.  Lines are modeled by their sizes;        *)
(* commit_prefix is the server loop.                                     *)
(* --------------------------------------------------------------------- *)

Fixpoint iterate {A : Type} (f : A -> A) (n : nat) (x : A) : A :=
  match n with
  | 0 => x
  | S n' => f (iterate f n' x)
  end.

Fixpoint commit_prefix (bound : nat) (ls : list nat) : list nat * bool :=
  match ls with
  | [] => ([], true)
  | l :: t => if l <=? bound
              then let '(c, ok) := commit_prefix bound t in (l :: c, ok)
              else ([], false)
  end.

Definition push_attempt_off (bound : nat) (f : list nat) (o : nat) : nat :=
  let '(_, ok) := commit_prefix bound (skipn o f) in
  if ok then length f else o.

Theorem oversize_line_wedges_push :
  (* sizes [1; 9; 2] against bound 4: the request fails (400) yet the   *)
  (* prefix [1] IS committed...                                          *)
  commit_prefix 4 [1; 9; 2] = ([1], false)
  (* ...and no number of retries ever advances the offset: the line of  *)
  (* size 2 behind the oversize one never syncs.                         *)
  /\ forall n, iterate (push_attempt_off 4 [1; 9; 2]) n 0 = 0.
Proof.
  split.
  - reflexivity.
  - induction n as [|n IH]; simpl.
    + reflexivity.
    + rewrite IH. reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* C7.  Mirror image on pull: one oversize STORED line wedges every pull *)
(* FLEET-WIDE.  The client scanner reading /log/since also caps lines at *)
(* MaxScanLine (pull.go:200-201, walk.go:16), and JSON escaping can      *)
(* nearly double a stored line on the wire -- so a ~3 MB line that pushed *)
(* fine can be impossible to pull.  pullLogs then errors before merging  *)
(* anything or advancing the cursor (pull.go:216-218, 65-68): every      *)
(* future pull on every client dies at the same line.  sz gives each     *)
(* line's wire size; pull_bounded is pullLogs.                           *)
(* --------------------------------------------------------------------- *)

Definition pull_bounded (sz : lline -> nat) (bound c : nat) (st : lstore)
  : option (list lline) :=
  if forallb (fun l => sz l <=? bound) (pull c st)
  then Some (pull c st) else None.

Theorem oversize_line_wedges_pull :
  exists (sz : lline -> nat) (bound : nat) (st : lstore) (l : lline),
    In l st /\ bound < sz l /\
    forall c, c < lseen l -> pull_bounded sz bound c st = None.
Proof.
  exists (fun _ => 9), 4, [mkLine (0, 0) 5], (mkLine (0, 0) 5).
  split; [left; reflexivity|].
  split; [lia|].
  intros c Hc. simpl in Hc.
  unfold pull_bounded, pull. simpl.
  apply Nat.ltb_lt in Hc. rewrite Hc.
  reflexivity.
Qed.

(* ===================================================================== *)
(* Part 7. Honesty lemmas D -- client-side blob behaviors the Part 1      *)
(* theorems do not endorse.                                              *)
(* ===================================================================== *)

(* --------------------------------------------------------------------- *)
(* D1.  "Push before you pull" RESURRECTS deletions.  Push builds its    *)
(* manifest from the walk alone (push.go:94-133) and generates           *)
(* tombstones only from its OWN KnownPushedBlobPaths (push.go:87-92);    *)
(* there is no check against server tombstones.  A host that still       *)
(* holds the old copy -- it never pulled the deletion -- and pushes first  *)
(* re-uploads it, and arrival order makes it the snapshot.  Nobody       *)
(* re-created anything: recreate_wins is the same math read as a         *)
(* feature; this lemma reads it as the failure it also is.  Every        *)
(* deletion is undone by any stale host that pushes before pulling.      *)
(* --------------------------------------------------------------------- *)

Theorem push_before_pull_resurrects :
  exists (server : list entry) (stale : sha),
    snapshot server = None /\                        (* deletion visible *)
    snapshot (ingest1 server (Content stale)) = Some stale.
Proof.
  exists [Content 1; Tomb], 1.
  split; reflexivity.
Qed.

(* --------------------------------------------------------------------- *)
(* D2.  The client cannot tell a RESET server from a tombstoned one --    *)
(* and reacts by deleting its local files.  Part 1's append-only         *)
(* theorems assume ONE immortal server history; the client has no epoch  *)
(* check (state.go, pull.go).  Against an empty / reset / restored-from- *)
(* backup DB, /current is empty and pull.go:50-59 os.Remove's EVERY      *)
(* path in KnownPulledBlobPaths.  Model-level, per path, "fresh server"  *)
(* and "everything tombstoned" are the SAME observation -- so the sweep,  *)
(* sound inside the model, is a mass delete against the wrong reality.   *)
(* --------------------------------------------------------------------- *)

Theorem server_reset_indistinguishable :
  forall history, snapshot ([] : list entry) = snapshot (history ++ [Tomb]).
Proof.
  intros history. rewrite snapshot_app. reflexivity.
Qed.

(* The sweep (pull.go:50-59): known-pulled paths absent from /current    *)
(* are removed locally.  Against an empty server, that is all of them.   *)
Definition swept_away (known : list nat) (on_server : nat -> bool)
  : list nat :=
  filter (fun p => negb (on_server p)) known.

Theorem server_reset_mass_delete :
  forall known, swept_away known (fun _ => false) = known.
Proof.
  intros known. unfold swept_away.
  induction known as [|p t IH]; simpl.
  - reflexivity.
  - f_equal. apply IH.
Qed.

(* --------------------------------------------------------------------- *)
(* D3.  Pull is "remote wins", unconditionally -- and watch runs it even  *)
(* after a FAILED push.  writeIfDifferent (pull.go:126-160) overwrites   *)
(* with no local-modification check; the tombstone sweep (pull.go:50-59) *)
(* removes files with no check either.  Honesty lemma A covered only     *)
(* the server-side arrival race; this is the client half.  The watch     *)
(* serializer runs Pull even when the Push that preceded it failed --     *)
(* the push error is only logged (serializer.go:36-49) -- so a cheka      *)
(* finding or a dead network blocks the push and the very next pull      *)
(* reverts or DELETES the unpushed local edit.  `cccp watch` is          *)
(* therefore not a safety mechanism for local edits.                     *)
(* --------------------------------------------------------------------- *)

(* Client-side application of a pulled snapshot to one path: local       *)
(* content is REPLACED by the server's view, whatever it was.            *)
Definition apply_pull (server_snap local : option sha) : option sha :=
  server_snap.

Definition serializer_cycle (push_succeeded : bool) (server : list entry)
    (pushed : entry) (local : option sha) : list entry * option sha :=
  let server' := if push_succeeded then ingest1 server pushed else server in
  (server', apply_pull (snapshot server') local).

Theorem watch_reverts_unpushed_edit :
  exists (server : list entry) (edit : sha),
    snapshot server <> Some edit /\   (* the edit was never pushed        *)
    (* push fails, watch pulls anyway: the locally modified file is GONE *)
    snd (serializer_cycle false server (Content edit) (Some edit)) = None /\
    Some edit <> None.
Proof.
  exists [Tomb], 1.
  split; [discriminate|].
  split; [reflexivity|].
  discriminate.
Qed.

(* --------------------------------------------------------------------- *)
(* D4.  One dangling symlink wedges push machine-wide.  resolveAndStat   *)
(* errors on a symlink whose target cannot be resolved (walk.go:110-118),*)
(* WalkDir propagates it (walk.go:90-100), and Push returns before       *)
(* anything is sent (push.go:63-66).  Unlike unreadable regular files    *)
(* (warn-and-skip, push.go:112-117), a broken link aborts the ENTIRE     *)
(* push, every time, until it is removed: no blob, log line or           *)
(* tombstone from that machine reaches the server, making every          *)
(* push-first obligation above unmeetable machine-wide.                  *)
(* --------------------------------------------------------------------- *)

Definition walk_result (dangling_link : bool) (files : list entry)
  : option (list entry) :=
  if dangling_link then None else Some files.

Definition push_or_abort (dangling_link : bool) (files : list entry)
    (server : list entry) : list entry :=
  match walk_result dangling_link files with
  | None => server                    (* Push aborts: nothing ingested *)
  | Some ms => ingest_seq server ms
  end.

Theorem dangling_symlink_wedges_push :
  forall files server n,
    iterate (push_or_abort true files) n server = server.
Proof.
  intros files server n.
  induction n as [|n IH]; simpl.
  - reflexivity.
  - rewrite IH. reflexivity.
Qed.
