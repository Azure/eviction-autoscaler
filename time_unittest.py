#!/usr/bin/env python3
# test_time_failure.py

import unittest
from datetime import datetime, timedelta
from dataclasses import dataclass
from typing import List, Optional

@dataclass
class Pod:
  name: str
  status: str
  start_time: datetime
  failure_time: Optional[datetime] = None
  
  @property
  def uptime(self) -> timedelta:
    """Calculate how long the pod ran before failing"""
    if self.failure_time and self.status in ['Failed', 'Error', 'CrashLoopBackOff']:
      return self.failure_time - self.start_time
    elif self.status == 'Running':
      return datetime.now() - self.start_time
    return timedelta(0)

def detect_time_failure_pattern(pods: List[Pod], expected_failure_time: timedelta = timedelta(minutes=10), tolerance: timedelta = timedelta(seconds=30)) -> List[Pod]:
  """
  Detect pods that follow a time-based failure pattern.
  
  Args:
    pods: List of pod objects to analyze
    expected_failure_time: Expected time before failure (default: 10 minutes)
    tolerance: Acceptable variance from expected failure time
  
  Returns:
    List of pods that match the time-failure pattern
  """
  matching_pods = []
  
  for pod in pods:
    if pod.status in ['Failed', 'Error', 'CrashLoopBackOff'] and pod.failure_time:
      uptime = pod.uptime
      time_diff = abs(uptime - expected_failure_time)
      
      if time_diff <= tolerance:
        matching_pods.append(pod)
  
  return matching_pods

class TestTimeFailurePattern(unittest.TestCase):
  
  def setUp(self):
    self.base_time = datetime.now()
  
  def test_detect_pods_failing_at_10_minutes(self):
    """Test detection of pods that fail after ~10 minutes"""
    pods = [
      Pod("pod-1", "Failed", self.base_time - timedelta(minutes=11), self.base_time - timedelta(minutes=1)),
      Pod("pod-2", "Error", self.base_time - timedelta(minutes=10, seconds=15), self.base_time - timedelta(seconds=15)),
      Pod("pod-3", "CrashLoopBackOff", self.base_time - timedelta(minutes=9, seconds=45), self.base_time + timedelta(seconds=15)),
    ]
    
    result = detect_time_failure_pattern(pods)
    self.assertEqual(len(result), 3)
    self.assertEqual(set(p.name for p in result), {"pod-1", "pod-2", "pod-3"})
  
  def test_ignore_pods_failing_at_different_times(self):
    """Test that pods failing at significantly different times are not detected"""
    pods = [
      Pod("pod-1", "Failed", self.base_time - timedelta(minutes=5), self.base_time),  # Failed after 5 min
      Pod("pod-2", "Error", self.base_time - timedelta(minutes=20), self.base_time - timedelta(minutes=5)),  # Failed after 15 min
      Pod("pod-3", "Failed", self.base_time - timedelta(hours=1), self.base_time),  # Failed after 1 hour
    ]
    
    result = detect_time_failure_pattern(pods)
    self.assertEqual(len(result), 0)
  
  def test_ignore_running_pods(self):
    """Test that running pods are not included in the pattern"""
    pods = [
      Pod("pod-1", "Running", self.base_time - timedelta(minutes=15)),
      Pod("pod-2", "Running", self.base_time - timedelta(minutes=5)),
      Pod("pod-3", "Failed", self.base_time - timedelta(minutes=10, seconds=5), self.base_time - timedelta(seconds=5)),
    ]
    
    result = detect_time_failure_pattern(pods)
    self.assertEqual(len(result), 1)
    self.assertEqual(result[0].name, "pod-3")
  
  def test_custom_failure_time_and_tolerance(self):
    """Test detection with custom expected failure time and tolerance"""
    pods = [
      Pod("pod-1", "Failed", self.base_time - timedelta(minutes=5, seconds=10), self.base_time - timedelta(seconds=10)),
      Pod("pod-2", "Error", self.base_time - timedelta(minutes=4, seconds=50), self.base_time + timedelta(seconds=10)),
      Pod("pod-3", "Failed", self.base_time - timedelta(minutes=10), self.base_time),  # Should not match
    ]
    
    result = detect_time_failure_pattern(pods, expected_failure_time=timedelta(minutes=5), tolerance=timedelta(seconds=15))
    self.assertEqual(len(result), 2)
    self.assertEqual(set(p.name for p in result), {"pod-1", "pod-2"})
  
  def test_empty_pod_list(self):
    """Test with empty pod list"""
    result = detect_time_failure_pattern([])
    self.assertEqual(len(result), 0)
  
  def test_pods_without_failure_time(self):
    """Test pods in failed state but without failure_time set"""
    pods = [
      Pod("pod-1", "Failed", self.base_time - timedelta(minutes=10)),
      Pod("pod-2", "Error", self.base_time - timedelta(minutes=10), self.base_time),
    ]
    
    result = detect_time_failure_pattern(pods)
    self.assertEqual(len(result), 1)
    self.assertEqual(result[0].name, "pod-2")

if __name__ == '__main__':
  unittest.main()