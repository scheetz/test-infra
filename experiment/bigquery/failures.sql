#standardSQL
select /* Find jobs that have not passed in a long time */
  jobs.job,
  latest_pass, /* how recently did this job pass */
  weekly_builds,  /* how many times a week does it run */
  first_run, /* when is the first time it ran */
  latest_run,  /* when is the most recent run */
  DATE_DIFF(current_date(), if(latest_pass is null, first_run, date(latest_pass)), DAY) broken_days
from (
  select /* filter to jobs that ran this week */
    job,
    count(1) weekly_builds
  from `k8s-gubernator.build.all`
  where
    started > timestamp_sub(current_timestamp(), interval 7 day)
  group by job
  order by job
) jobs
left join (
  select /* find the oldest, newest run of each job */
    job,
    date(min(started)) first_run,
    date(max(started)) latest_run
  from `k8s-gubernator.build.all`
  group by job
) runs
on jobs.job = runs.job
left join (
  select /* find the most recent time each job passed (may not be this week) */
    job,
    max(started) latest_pass
  from `k8s-gubernator.build.all`
  where
    result = 'SUCCESS'
  group by job
) passes
on jobs.job = passes.job
where
  latest_pass is null
  or DATE_DIFF(current_date(), date(latest_pass), day) > 30
order by broken_days desc, latest_pass, first_run, weekly_builds desc, jobs.job
