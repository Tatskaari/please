package build.please.java.test;

import org.junit.Test;
import static org.junit.Assert.assertEquals;

public class SrcDirTest {
  @Test
  public void TestTheAnswer() {
    System.out.println("Test");
    assertEquals(42, SrcDirLib.WhatDoYouGetWhenYouMultiply(6, 9));
  }
}
